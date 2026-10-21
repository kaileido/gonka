// Package tokenizer estimates a prompt's tokens with the tokenizer.json a model's pinned Hugging Face revision ships.
// See README.md.
package tokenizer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	"devshard/cmd/gateway/filters"
	"devshard/cmd/gateway/internal/logkey"
	"devshard/logging"
)

const (
	huggingFaceBaseURL = "https://huggingface.co"
	// maxTokenizerBytes bounds one tokenizer.json read; the largest pinned today is about 20 MB.
	maxTokenizerBytes = 64 << 20
	loadTimeout       = 2 * time.Minute
	retryInterval     = 10 * time.Minute
)

// errNoTokenizer is a pinned revision without a tokenizer.json; a pinned commit never changes, so it is not fetched again.
var errNoTokenizer = errors.New("pinned revision has no tokenizer.json")

// ModelSource returns the Hugging Face repo and commit the chain pins for a model in an epoch; vocabulary.ChainModelSource satisfies it.
type ModelSource interface {
	GetModelSource(ctx context.Context, epochID uint64, modelID string) (hfRepo, hfCommit string, err error)
}

// Library holds the tokenizer the chain pins for each model it is asked about, loading each in the background on first use.
type Library struct {
	source     ModelSource
	epoch      func() uint64
	baseURL    string
	httpClient *http.Client
	now        func() time.Time

	mu     sync.Mutex
	models map[string]*pinnedTokenizer
}

type revision struct{ repo, commit string }

// pinnedTokenizer is one model's tokenizer as of the epoch its pin was last read for; counter is nil while none is loaded.
type pinnedTokenizer struct {
	epoch    uint64
	revision revision
	counter  *Counter
	loading  bool
	retryAt  time.Time
}

// NewLibrary builds a library that reads each model's pin on chain for the epoch the epoch func returns, 0 while unknown, and the tokenizer from the Hugging Face hub.
func NewLibrary(source ModelSource, epoch func() uint64) *Library {
	return &Library{
		source:     source,
		epoch:      epoch,
		baseURL:    huggingFaceBaseURL,
		httpClient: &http.Client{},
		now:        time.Now,
		models:     make(map[string]*pinnedTokenizer),
	}
}

// Estimate returns the estimated tokens of a normalized chat body's prompt; known is false while the model has no tokenizer loaded or the count fails.
func (l *Library) Estimate(model string, body []byte) (tokens uint64, known bool) {
	counter := l.counter(model)
	if counter == nil {
		return 0, false
	}
	prompt, err := filters.PromptText(body)
	if err != nil {
		return 0, false
	}
	tokens, err = counter.Estimate(prompt)
	return tokens, err == nil
}

// counter returns the model's loaded tokenizer, and starts reading its pin when the epoch moved on or a retry is due.
func (l *Library) counter(model string) *Counter {
	epoch := l.epoch()
	if epoch == 0 {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	entry, found := l.models[model]
	if !found {
		entry = &pinnedTokenizer{}
		l.models[model] = entry
	}
	if entry.epoch != epoch && !entry.loading && !l.now().Before(entry.retryAt) {
		entry.loading = true
		go l.refresh(model, epoch, entry.revision)
	}
	return entry.counter
}

// refresh reads the model's pin for epoch and loads its tokenizer unless the pin is the one already held.
func (l *Library) refresh(model string, epoch uint64, held revision) {
	ctx, cancel := context.WithTimeout(context.Background(), loadTimeout)
	defer cancel()
	started := l.now()
	repo, commit, err := l.source.GetModelSource(ctx, epoch, model)
	pinned := revision{repo: repo, commit: commit}
	if err == nil && (repo == "" || commit == "") {
		err = errNoTokenizer
	}
	if err != nil && !errors.Is(err, errNoTokenizer) {
		l.failed(model, err)
		return
	}
	if err == nil && pinned == held {
		l.settle(model, epoch, pinned, nil, false)
		return
	}
	var counter *Counter
	if err == nil {
		counter, err = l.load(ctx, pinned)
	}
	switch {
	case err == nil:
		logging.Info("prompt tokenizer loaded", logkey.Model, model, logkey.Tokenizer, pinned.repo+"@"+pinned.commit, logkey.DurationMS, l.now().Sub(started).Milliseconds())
		l.settle(model, epoch, pinned, counter, true)
	case errors.Is(err, errNoTokenizer), errors.Is(err, ErrUnsupported):
		logging.Info("model has no tokenizer the gateway reads; its prompts are not checked against its context length", logkey.Model, model, logkey.Tokenizer, pinned.repo+"@"+pinned.commit, logkey.Error, err)
		l.settle(model, epoch, pinned, nil, true)
	default:
		l.failed(model, err)
	}
}

func (l *Library) settle(model string, epoch uint64, pinned revision, counter *Counter, replace bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry := l.models[model]
	entry.epoch, entry.revision, entry.loading = epoch, pinned, false
	if replace {
		entry.counter = counter
	}
}

// failed keeps the tokenizer already held, which is the model's previous pin, until the retry is due.
func (l *Library) failed(model string, err error) {
	logging.Warn("prompt tokenizer could not be loaded; prompts are not checked until it is", logkey.Model, model, logkey.Error, err)
	l.mu.Lock()
	defer l.mu.Unlock()
	entry := l.models[model]
	entry.loading = false
	entry.retryAt = l.now().Add(retryInterval)
}

func (l *Library) load(ctx context.Context, pinned revision) (*Counter, error) {
	address, err := url.JoinPath(l.baseURL, pinned.repo, "resolve", pinned.commit, "tokenizer.json")
	if err != nil {
		return nil, fmt.Errorf("tokenizer url: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, fmt.Errorf("tokenizer request: %w", err)
	}
	response, err := l.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", address, err)
	}
	defer func() { _ = response.Body.Close() }()
	switch {
	case response.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("fetch %s: %w", address, errNoTokenizer)
	case response.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("fetch %s: status %d", address, response.StatusCode)
	case response.ContentLength > maxTokenizerBytes:
		return nil, fmt.Errorf("%w: %s is %d bytes", ErrUnsupported, address, response.ContentLength)
	}
	counter, err := Load(io.LimitReader(response.Body, maxTokenizerBytes))
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", address, err)
	}
	return counter, nil
}

// Package tokenizer counts a prompt's tokens with the tokenizer.json a model's pinned Hugging Face revision ships; see README.md.
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

// See README.md, "Where the tokenizer comes from" and "Bounds".
const (
	maxTokenizerBytes = 64 << 20
	loadTimeout       = 2 * time.Minute
	retryInterval     = 10 * time.Minute
	epochUnknown      = 0
	unpinned          = ""
)

var errNoTokenizer = errors.New("pinned revision has no tokenizer.json")

// ModelSource returns the Hugging Face repo and commit the chain pins for a model in an epoch; vocabulary.ChainModelSource satisfies it.
type ModelSource interface {
	GetModelSource(ctx context.Context, epochID uint64, modelID string) (hfRepo, hfCommit string, err error)
}

// Library holds the tokenizer the chain pins for each model it is asked about, loading each in the background on first use.
type Library struct {
	source     ModelSource
	epoch      func() uint64
	httpClient *http.Client
	now        func() time.Time
	background func(task func())

	mu     sync.Mutex
	models map[string]*pinnedTokenizer
}

type revision struct{ repo, commit string }

func (r revision) String() string { return r.repo + "@" + r.commit }

type pinnedTokenizer struct {
	epoch    uint64
	revision revision
	counter  *Counter
	loading  bool
	retryAt  time.Time
}

func (p *pinnedTokenizer) startRefresh(epoch uint64, now time.Time) bool {
	if p.epoch == epoch || p.loading || now.Before(p.retryAt) {
		return false
	}
	p.loading = true
	return true
}

func (p *pinnedTokenizer) settle(epoch uint64, pinned revision, counter *Counter) {
	p.epoch, p.revision, p.counter, p.loading = epoch, pinned, counter, false
}

func (p *pinnedTokenizer) retryAfter(at time.Time) {
	p.loading, p.retryAt = false, at
}

// NewLibrary builds a library that reads each model's pin on chain for the epoch the epoch func returns, nothing while it returns 0, and the tokenizer from the Hugging Face hub.
func NewLibrary(source ModelSource, epoch func() uint64) *Library {
	return &Library{
		source:     source,
		epoch:      epoch,
		httpClient: &http.Client{},
		now:        time.Now,
		background: func(task func()) { go task() },
		models:     make(map[string]*pinnedTokenizer),
	}
}

// MinimumPromptTokens returns the fewest tokens a normalized chat body's messages hold; known is false while the model has no tokenizer loaded or the count fails.
func (l *Library) MinimumPromptTokens(model string, body []byte) (tokens uint64, known bool) {
	counter := l.counter(model)
	if counter == nil {
		return 0, false
	}
	prompt, err := filters.PromptText(body)
	if err != nil {
		return 0, false
	}
	tokens, err = counter.MinimumTokens(prompt)
	return tokens, err == nil
}

func (l *Library) counter(model string) *Counter {
	epoch := l.epoch()
	if epoch == epochUnknown {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	entry, found := l.models[model]
	if !found {
		entry = &pinnedTokenizer{}
		l.models[model] = entry
	}
	if entry.startRefresh(epoch, l.now()) {
		held := entry.revision
		l.background(func() { l.refresh(model, epoch, held) })
	}
	return entry.counter
}

func (l *Library) refresh(model string, epoch uint64, held revision) {
	ctx, cancel := context.WithTimeout(context.Background(), loadTimeout)
	defer cancel()
	started := l.now()
	pinned, err := l.readPin(ctx, model, epoch)
	if err == nil && pinned == held {
		l.update(model, func(entry *pinnedTokenizer) { entry.settle(epoch, held, entry.counter) })
		return
	}
	var counter *Counter
	if err == nil {
		counter, err = l.load(ctx, pinned)
	}
	switch {
	case err == nil:
		logging.Info("prompt tokenizer loaded", logkey.Model, model, logkey.Tokenizer, pinned.String(), logkey.DurationMS, l.now().Sub(started).Milliseconds())
	case errors.Is(err, errNoTokenizer), errors.Is(err, ErrUnsupported):
		logging.Info("model has no tokenizer the gateway reads; its prompts are not checked against its context length", logkey.Model, model, logkey.Tokenizer, pinned.String(), logkey.Error, err)
	default:
		logging.Warn("prompt tokenizer could not be loaded; prompts are not checked until it is", logkey.Model, model, logkey.Error, err)
		l.update(model, func(entry *pinnedTokenizer) { entry.retryAfter(l.now().Add(retryInterval)) })
		return
	}
	l.update(model, func(entry *pinnedTokenizer) { entry.settle(epoch, pinned, counter) })
}

func (l *Library) readPin(ctx context.Context, model string, epoch uint64) (revision, error) {
	repo, commit, err := l.source.GetModelSource(ctx, epoch, model)
	pinned := revision{repo: repo, commit: commit}
	if err == nil && (repo == unpinned || commit == unpinned) {
		err = errNoTokenizer
	}
	return pinned, err
}

func (l *Library) update(model string, change func(entry *pinnedTokenizer)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	change(l.models[model])
}

func (l *Library) load(ctx context.Context, pinned revision) (*Counter, error) {
	address, err := url.JoinPath(huggingFaceBaseURL, pinned.repo, hubResolveSegment, pinned.commit, tokenizerFileName)
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

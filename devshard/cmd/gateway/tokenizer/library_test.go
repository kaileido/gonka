package tokenizer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeSource struct {
	commit string
	err    error
	reads  int
}

func (s *fakeSource) GetModelSource(_ context.Context, _ uint64, model string) (string, string, error) {
	s.reads++
	return "org/" + model, s.commit, s.err
}

type fakeHub struct {
	status  int
	fetches int
	body    []byte
}

func newFakeHub(t *testing.T) *fakeHub {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", "isolated.json"))
	if err != nil {
		t.Fatal(err)
	}
	return &fakeHub{status: http.StatusOK, body: body}
}

func (h *fakeHub) RoundTrip(request *http.Request) (*http.Response, error) {
	h.fetches++
	status, body := h.status, h.body
	if request.URL.Host != "huggingface.co" || !strings.HasPrefix(request.URL.Path, "/org/m/resolve/") || !strings.HasSuffix(request.URL.Path, "/tokenizer.json") {
		status = http.StatusNotFound
	}
	if status != http.StatusOK {
		body = nil
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body)), Request: request}, nil
}

type testClock struct{ now time.Time }

func (c *testClock) read() time.Time          { return c.now }
func (c *testClock) advance(by time.Duration) { c.now = c.now.Add(by) }

type testLibrary struct {
	*Library
	epoch   uint64
	pending []func()
}

func newTestLibrary(source ModelSource, hub *fakeHub, epoch uint64, clock *testClock) *testLibrary {
	library := &testLibrary{epoch: epoch}
	library.Library = NewLibrary(source, func() uint64 { return library.epoch })
	library.httpClient = &http.Client{Transport: hub}
	library.now = clock.read
	library.background = func(task func()) { library.pending = append(library.pending, task) }
	return library
}

func (l *testLibrary) runPending() int {
	tasks := l.pending
	l.pending = nil
	for _, task := range tasks {
		task()
	}
	return len(tasks)
}

const chatBody = `{"model":"m","messages":[{"role":"user","content":"The quick brown fox jumps over the lazy dog."}]}`

// Test flow:
//  1. Build a library over a fake chain pin and a fake hub serving a tokenizer.json at the pinned commit.
//  2. Count a body: assert the first call is unknown and queues one load.
//  3. Run the load and count again: assert the count is the tokenizer's exact count of the message.
//  4. Move to a new epoch that pins the same commit: assert the pin is read again and the tokenizer is neither fetched again nor dropped.
//  5. Pin a new commit in the next epoch: assert the tokenizer is fetched again.
func TestLibraryLoadsThePinnedTokenizerOnceAPin(t *testing.T) {
	source := &fakeSource{commit: "c1"}
	hub := newFakeHub(t)
	library := newTestLibrary(source, hub, 5, &testClock{})

	if _, known := library.MinimumPromptTokens("m", []byte(chatBody)); known {
		t.Fatal("the first count is known before any tokenizer loaded")
	}
	if loads := library.runPending(); loads != 1 {
		t.Fatalf("loads queued = %d, want 1", loads)
	}
	tokens, known := library.MinimumPromptTokens("m", []byte(chatBody))
	want, err := loadTestdata(t, "isolated").Count(goldenTexts[0])
	if err != nil {
		t.Fatal(err)
	}
	if !known || tokens != uint64(want) {
		t.Fatalf("MinimumPromptTokens = %d, %v, want %d, true", tokens, known, want)
	}

	library.epoch = 6
	library.MinimumPromptTokens("m", []byte(chatBody))
	library.runPending()
	if _, known := library.MinimumPromptTokens("m", []byte(chatBody)); !known || source.reads != 2 || hub.fetches != 1 {
		t.Fatalf("known = %v, pin reads = %d, fetches = %d after an epoch that kept the pin, want true, 2, 1", known, source.reads, hub.fetches)
	}

	source.commit = "c2"
	library.epoch = 7
	library.MinimumPromptTokens("m", []byte(chatBody))
	library.runPending()
	if hub.fetches != 2 {
		t.Fatalf("fetches = %d after the pin moved, want 2", hub.fetches)
	}
}

// Test flow:
//  1. Serve 404 for the pinned tokenizer.json, as for a model that ships none.
//  2. Count and run the load, then count again past the retry interval and in a new epoch with the same pin, running every queued load.
//  3. Assert every count is unknown and the hub was asked once: a pinned commit never gains a file.
func TestLibraryDoesNotRefetchAPinWithoutATokenizer(t *testing.T) {
	source := &fakeSource{commit: "c1"}
	hub := newFakeHub(t)
	hub.status = http.StatusNotFound
	clock := &testClock{}
	library := newTestLibrary(source, hub, 5, clock)

	library.MinimumPromptTokens("m", []byte(chatBody))
	library.runPending()
	clock.advance(2 * retryInterval)
	library.epoch = 6
	_, known := library.MinimumPromptTokens("m", []byte(chatBody))
	library.runPending()

	if known || source.reads != 2 || hub.fetches != 1 {
		t.Fatalf("known = %v, pin reads = %d, fetches = %d, want false, 2, 1", known, source.reads, hub.fetches)
	}
}

// Test flow:
//  1. Fail the first load with a 503 from the hub.
//  2. Assert another count inside the retry interval queues no load.
//  3. Restore the hub, step past the retry interval, and assert the next count queues a load that makes the count known.
func TestLibraryRetriesATransientFailureAfterTheInterval(t *testing.T) {
	source := &fakeSource{commit: "c1"}
	hub := newFakeHub(t)
	hub.status = http.StatusServiceUnavailable
	clock := &testClock{}
	library := newTestLibrary(source, hub, 5, clock)

	library.MinimumPromptTokens("m", []byte(chatBody))
	library.runPending()
	library.MinimumPromptTokens("m", []byte(chatBody))
	if loads := library.runPending(); loads != 0 || hub.fetches != 1 {
		t.Fatalf("loads = %d, fetches = %d inside the retry interval, want 0, 1", loads, hub.fetches)
	}

	hub.status = http.StatusOK
	clock.advance(retryInterval)
	library.MinimumPromptTokens("m", []byte(chatBody))
	library.runPending()
	if _, known := library.MinimumPromptTokens("m", []byte(chatBody)); !known {
		t.Fatal("the count is unknown after the retry loaded the tokenizer")
	}
}

// Test flow:
//  1. Build a library whose chain epoch is not known yet, and one whose pin read fails.
//  2. Count on each, running every queued load.
//  3. Assert both are unknown, the first without reading a pin at all.
func TestLibraryWithoutAPinCountsNothing(t *testing.T) {
	hub := newFakeHub(t)
	unread := &fakeSource{commit: "c1"}
	early := newTestLibrary(unread, hub, epochUnknown, &testClock{})
	_, known := early.MinimumPromptTokens("m", []byte(chatBody))
	if loads := early.runPending(); known || loads != 0 || unread.reads != 0 {
		t.Fatalf("known = %v, loads = %d, pin reads = %d before the epoch is known, want false, 0, 0", known, loads, unread.reads)
	}

	library := newTestLibrary(&fakeSource{err: errors.New("chain down")}, hub, 5, &testClock{})
	library.MinimumPromptTokens("m", []byte(chatBody))
	library.runPending()
	if _, known := library.MinimumPromptTokens("m", []byte(chatBody)); known {
		t.Fatal("a model whose pin could not be read is counted")
	}
}

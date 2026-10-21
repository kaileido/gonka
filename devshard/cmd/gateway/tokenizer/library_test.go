package tokenizer

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type fakeSource struct {
	mu     sync.Mutex
	commit string
	err    error
	reads  atomic.Int64
}

func (s *fakeSource) GetModelSource(_ context.Context, _ uint64, model string) (string, string, error) {
	s.reads.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	return "org/" + model, s.commit, s.err
}

func (s *fakeSource) pin(commit string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commit = commit
}

type fakeHub struct {
	status  atomic.Int64
	fetches atomic.Int64
	body    []byte
}

func newFakeHub(t *testing.T) (*fakeHub, *httptest.Server) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", "isolated.json"))
	if err != nil {
		t.Fatal(err)
	}
	hub := &fakeHub{body: body}
	hub.status.Store(http.StatusOK)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hub.fetches.Add(1)
		if !strings.HasPrefix(r.URL.Path, "/org/m/resolve/") || !strings.HasSuffix(r.URL.Path, "/tokenizer.json") {
			http.NotFound(w, r)
			return
		}
		if status := int(hub.status.Load()); status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		_, _ = w.Write(hub.body)
	}))
	t.Cleanup(server.Close)
	return hub, server
}

type testClock struct{ now atomic.Int64 }

func (c *testClock) read() time.Time          { return time.Unix(0, c.now.Load()) }
func (c *testClock) advance(by time.Duration) { c.now.Add(int64(by)) }

func newTestLibrary(source ModelSource, hub *httptest.Server, epoch *atomic.Uint64, clock *testClock) *Library {
	library := NewLibrary(source, epoch.Load)
	library.baseURL = hub.URL
	library.httpClient = hub.Client()
	library.now = clock.read
	return library
}

const chatBody = `{"model":"m","messages":[{"role":"user","content":"The quick brown fox jumps over the lazy dog."}]}`

func settled(library *Library, model string) bool {
	library.mu.Lock()
	defer library.mu.Unlock()
	entry, found := library.models[model]
	return found && !entry.loading
}

// Test flow:
//  1. Build a library over a fake chain pin and a fake hub serving a tokenizer.json at the pinned commit.
//  2. Estimate a body: assert the first call is unknown and starts the load in the background.
//  3. Wait for the load and estimate again: assert the count is the tokenizer's exact count of the message.
//  4. Move to a new epoch that pins the same commit: assert the pin is read again and the tokenizer is not fetched again.
//  5. Pin a new commit in the next epoch: assert the tokenizer is fetched again.
func TestLibraryLoadsThePinnedTokenizerOnceAPin(t *testing.T) {
	source := &fakeSource{commit: "c1"}
	hub, server := newFakeHub(t)
	var epoch atomic.Uint64
	epoch.Store(5)
	library := newTestLibrary(source, server, &epoch, &testClock{})

	if _, known := library.MinimumPromptTokens("m", []byte(chatBody)); known {
		t.Fatal("the first estimate is known before any tokenizer loaded")
	}
	require.Eventually(t, func() bool { return settled(library, "m") }, 10*time.Second, time.Millisecond)
	tokens, known := library.MinimumPromptTokens("m", []byte(chatBody))
	want, err := loadTestdata(t, "isolated").Count(goldenTexts[0])
	if err != nil {
		t.Fatal(err)
	}
	if !known || tokens != uint64(want) {
		t.Fatalf("Estimate = %d, %v, want %d, true", tokens, known, want)
	}

	epoch.Store(6)
	library.MinimumPromptTokens("m", []byte(chatBody))
	require.Eventually(t, func() bool { return settled(library, "m") && source.reads.Load() == 2 }, 10*time.Second, time.Millisecond)
	if hub.fetches.Load() != 1 {
		t.Fatalf("fetches = %d after an epoch that kept the pin, want 1", hub.fetches.Load())
	}
	if _, known := library.MinimumPromptTokens("m", []byte(chatBody)); !known {
		t.Fatal("the tokenizer was dropped by an epoch that kept the pin")
	}

	source.pin("c2")
	epoch.Store(7)
	library.MinimumPromptTokens("m", []byte(chatBody))
	require.Eventually(t, func() bool { return settled(library, "m") && hub.fetches.Load() == 2 }, 10*time.Second, time.Millisecond)
}

// Test flow:
//  1. Serve 404 for the pinned tokenizer.json, as for a model that ships none.
//  2. Estimate, wait for the load, then estimate again across the retry interval and a new epoch with the same pin.
//  3. Assert every estimate is unknown and the hub was asked once: a pinned commit never gains a file.
func TestLibraryDoesNotRefetchAPinWithoutATokenizer(t *testing.T) {
	source := &fakeSource{commit: "c1"}
	hub, server := newFakeHub(t)
	hub.status.Store(http.StatusNotFound)
	var epoch atomic.Uint64
	epoch.Store(5)
	clock := &testClock{}
	library := newTestLibrary(source, server, &epoch, clock)

	library.MinimumPromptTokens("m", []byte(chatBody))
	require.Eventually(t, func() bool { return settled(library, "m") }, 10*time.Second, time.Millisecond)
	clock.advance(2 * retryInterval)
	epoch.Store(6)
	if _, known := library.MinimumPromptTokens("m", []byte(chatBody)); known {
		t.Fatal("a model without a tokenizer is estimated")
	}
	require.Eventually(t, func() bool { return settled(library, "m") && source.reads.Load() == 2 }, 10*time.Second, time.Millisecond)

	if hub.fetches.Load() != 1 {
		t.Fatalf("fetches = %d, want 1", hub.fetches.Load())
	}
}

// Test flow:
//  1. Fail the first load with a 503 from the hub.
//  2. Assert another estimate inside the retry interval starts no load.
//  3. Restore the hub, step past the retry interval, and assert the next estimate loads the tokenizer.
func TestLibraryRetriesATransientFailureAfterTheInterval(t *testing.T) {
	source := &fakeSource{commit: "c1"}
	hub, server := newFakeHub(t)
	hub.status.Store(http.StatusServiceUnavailable)
	var epoch atomic.Uint64
	epoch.Store(5)
	clock := &testClock{}
	library := newTestLibrary(source, server, &epoch, clock)

	library.MinimumPromptTokens("m", []byte(chatBody))
	require.Eventually(t, func() bool { return settled(library, "m") }, 10*time.Second, time.Millisecond)
	library.MinimumPromptTokens("m", []byte(chatBody))
	if !settled(library, "m") || hub.fetches.Load() != 1 {
		t.Fatalf("fetches = %d inside the retry interval, want 1", hub.fetches.Load())
	}

	hub.status.Store(http.StatusOK)
	clock.advance(retryInterval)
	library.MinimumPromptTokens("m", []byte(chatBody))
	require.Eventually(t, func() bool {
		_, known := library.MinimumPromptTokens("m", []byte(chatBody))
		return known
	}, 10*time.Second, time.Millisecond)
}

// Test flow:
//  1. Build a library whose chain epoch is not known yet, and one whose pin read fails.
//  2. Estimate on each.
//  3. Assert both are unknown, the first without reading a pin at all.
func TestLibraryWithoutAPinEstimatesNothing(t *testing.T) {
	_, server := newFakeHub(t)
	unread := &fakeSource{commit: "c1"}
	var unknownEpoch atomic.Uint64
	if _, known := newTestLibrary(unread, server, &unknownEpoch, &testClock{}).MinimumPromptTokens("m", []byte(chatBody)); known || unread.reads.Load() != 0 {
		t.Fatalf("known = %v, pin reads = %d before the epoch is known, want false, 0", known, unread.reads.Load())
	}

	failing := &fakeSource{err: errors.New("chain down")}
	var epoch atomic.Uint64
	epoch.Store(5)
	library := newTestLibrary(failing, server, &epoch, &testClock{})
	library.MinimumPromptTokens("m", []byte(chatBody))
	require.Eventually(t, func() bool { return settled(library, "m") }, 10*time.Second, time.Millisecond)
	if _, known := library.MinimumPromptTokens("m", []byte(chatBody)); known {
		t.Fatal("a model whose pin could not be read is estimated")
	}
}

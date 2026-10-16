package chain

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"common/httpguard"

	"devshard/cmd/gateway/internal/leakcheck"
)

// fakeClock is a mutex-guarded injectable clock so TTL/staleness tests never
// depend on the real wall clock.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(start time.Time) *fakeClock {
	return &fakeClock{now: start}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// newVersionsStub starts an httptest server serving a fixed /v1/versions
// response; it is closed automatically on test cleanup.
func newVersionsStub(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		if body != "" {
			w.Write([]byte(body))
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// Test flow:
//  1. Start a versions stub reporting one PoC-validation-capable node and a fake clock at time zero.
//  2. Create a VersionsCache with a one-minute TTL against the stub and register the candidate miner.
//  3. Poll the cache.
//  4. Assert the node reads as validation-capable within the TTL.
func TestVersionsCache_PollSucceeds_NodeCapableWithinTTL(t *testing.T) {
	server := newVersionsStub(t, http.StatusOK, `{"mlnodes":[{"node_id":"node-1","poc_validation_inference":true}]}`)
	clock := newFakeClock(time.Unix(0, 0))
	cache := NewVersionsCache(server.Client(), time.Minute, clock.Now)
	cache.SetCandidates(map[string]string{"miner-1": server.URL})

	cache.Poll(context.Background())

	if !cache.IsNodeValidationCapable("miner-1", "node-1") {
		t.Fatal("capable node within ttl should be validation-capable")
	}
}

// Test flow:
//  1. Create a VersionsCache with a fake clock and no registered candidates.
//  2. Assert IsNodeValidationCapable for a miner that was never polled returns false (fails closed).
func TestVersionsCache_UnknownMiner_FailsClosed(t *testing.T) {
	clock := newFakeClock(time.Unix(0, 0))
	cache := NewVersionsCache(http.DefaultClient, time.Minute, clock.Now)

	if cache.IsNodeValidationCapable("never-a-candidate", "node-1") {
		t.Fatal("miner never polled should fail closed")
	}
}

// Test flow:
//  1. Start a versions stub returning HTTP 500 and a fake clock at time zero.
//  2. Create a VersionsCache against the stub and register the candidate miner.
//  3. Poll the cache.
//  4. Assert the node reads as not validation-capable (fails closed) after the failed poll.
func TestVersionsCache_ServerError_FailsClosed(t *testing.T) {
	server := newVersionsStub(t, http.StatusInternalServerError, "")
	clock := newFakeClock(time.Unix(0, 0))
	cache := NewVersionsCache(server.Client(), time.Minute, clock.Now)
	cache.SetCandidates(map[string]string{"miner-1": server.URL})

	cache.Poll(context.Background())

	if cache.IsNodeValidationCapable("miner-1", "node-1") {
		t.Fatal("500 response should fail closed")
	}
}

// Test flow:
//  1. Start a versions stub reporting one node with poc_validation_inference=false, and a fake clock.
//  2. Create a VersionsCache against the stub and register the candidate miner.
//  3. Poll the cache.
//  4. Assert the node reads as not validation-capable.
func TestVersionsCache_NodeExplicitlyIncapable_ReturnsFalse(t *testing.T) {
	server := newVersionsStub(t, http.StatusOK, `{"mlnodes":[{"node_id":"node-1","poc_validation_inference":false}]}`)
	clock := newFakeClock(time.Unix(0, 0))
	cache := NewVersionsCache(server.Client(), time.Minute, clock.Now)
	cache.SetCandidates(map[string]string{"miner-1": server.URL})

	cache.Poll(context.Background())

	if cache.IsNodeValidationCapable("miner-1", "node-1") {
		t.Fatal("node explicitly flagged poc_validation_inference=false should not be capable")
	}
}

// Test flow:
//  1. Start a versions stub reporting one capable node and a fake clock at time zero.
//  2. Create a VersionsCache with a one-minute TTL, register the candidate miner, and poll.
//  3. Assert the node is capable before the TTL elapses (precondition).
//  4. Advance the clock past the TTL.
//  5. Assert the node now reads as not validation-capable (fails closed).
func TestVersionsCache_EntryOlderThanTTL_FailsClosed(t *testing.T) {
	server := newVersionsStub(t, http.StatusOK, `{"mlnodes":[{"node_id":"node-1","poc_validation_inference":true}]}`)
	clock := newFakeClock(time.Unix(0, 0))
	cache := NewVersionsCache(server.Client(), time.Minute, clock.Now)
	cache.SetCandidates(map[string]string{"miner-1": server.URL})
	cache.Poll(context.Background())
	if !cache.IsNodeValidationCapable("miner-1", "node-1") {
		t.Fatal("precondition: node should be capable before the ttl elapses")
	}

	clock.Advance(2 * time.Minute)

	if cache.IsNodeValidationCapable("miner-1", "node-1") {
		t.Fatal("entry older than ttl should fail closed")
	}
}

// Test flow:
//  1. Start a versions stub reporting one capable node and a fake clock, register the candidate miner, and poll.
//  2. Assert the node is capable before its miner is removed (precondition).
//  3. Call SetCandidates again without that miner.
//  4. Assert the node now reads as not validation-capable even though its cached entry is still fresh.
func TestVersionsCache_SetCandidates_DropsRemovedMiner(t *testing.T) {
	server := newVersionsStub(t, http.StatusOK, `{"mlnodes":[{"node_id":"node-1","poc_validation_inference":true}]}`)
	clock := newFakeClock(time.Unix(0, 0))
	cache := NewVersionsCache(server.Client(), time.Minute, clock.Now)
	cache.SetCandidates(map[string]string{"miner-1": server.URL})
	cache.Poll(context.Background())
	if !cache.IsNodeValidationCapable("miner-1", "node-1") {
		t.Fatal("precondition: node should be capable before its miner is removed")
	}

	cache.SetCandidates(map[string]string{"miner-2": server.URL})

	if cache.IsNodeValidationCapable("miner-1", "node-1") {
		t.Fatal("miner dropped from candidates must fail closed even with a cached entry")
	}
}

// Test flow:
//  1. Register a leak check deferred first so it runs last, after the server and its client connections are closed.
//  2. Start a versions stub server, a fake clock, and a VersionsCache with one candidate miner.
//  3. Start Run in a goroutine against a cancellable context, then cancel the context.
//  4. Assert Run's goroutine exits within 2 seconds of the cancellation.
func TestVersionsCache_Run_ExitsOnContextCancel(t *testing.T) {
	defer leakcheck.VerifyNone(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"mlnodes":[{"node_id":"node-1","poc_validation_inference":true}]}`))
	}))
	defer server.Close()

	clock := newFakeClock(time.Unix(0, 0))
	cache := NewVersionsCache(server.Client(), time.Minute, clock.Now)
	cache.SetCandidates(map[string]string{"miner-1": server.URL})

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		cache.Run(ctx, time.Millisecond)
	}()

	cancel()

	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit within 2s of context cancellation")
	}
}

// Test flow:
//  1. Start a server whose handler reports arrival then blocks until released, and register a release-once deferred before server.Close so it runs first and a failed assertion can't leave handlers parked blocking Close.
//  2. Create a VersionsCache with several candidate miners all pointing at that server.
//  3. Start Poll in a goroutine.
//  4. Within a deadline shorter than one fetch's timeout, wait for every candidate's request to arrive, asserting they could not all have arrived if fetches were serialized one per timeout.
//  5. Release all handlers and assert Poll returns.
func TestVersionsPollFetchesConcurrently(t *testing.T) {
	defer leakcheck.VerifyNone(t)

	const candidateCount = 8
	arrived := make(chan struct{}, candidateCount)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrived <- struct{}{}
		<-release
		w.Write([]byte(`{"ml_nodes":[]}`))
	}))
	defer server.Close()
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseAll()

	clock := &fakeClock{now: time.Unix(0, 0)}
	cache := NewVersionsCache(server.Client(), time.Minute, clock.Now)
	candidates := make(map[string]string, candidateCount)
	for i := range candidateCount {
		candidates[fmt.Sprintf("miner-%d", i)] = server.URL
	}
	cache.SetCandidates(candidates)

	polled := make(chan struct{})
	go func() { defer close(polled); cache.Poll(context.Background()) }()

	allStarted := time.After(versionsFetchTimeout / 2)
	for i := range candidateCount {
		select {
		case <-arrived:
		case <-allStarted:
			t.Fatalf("only %d of %d fetches had started within %v; the pass is serialized", i, candidateCount, versionsFetchTimeout/2)
		}
	}
	releaseAll()

	select {
	case <-polled:
	case <-time.After(3 * time.Second):
		t.Fatal("Poll did not return after every fetch was released")
	}
}

// Test flow:
//  1. Start a loopback stub that counts hits, build an observer whose chain API client is the stub's unguarded client the versions poll used to share, and close the dial guard.
//  2. Register one miner by the stub's IP and one by the host name localhost as versions candidates.
//  3. Poll the observer's versions cache.
//  4. Assert the stub was never reached.
func TestTheVersionsPollDoesNotDialAMinerOnAPrivateAddress(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Write([]byte(`{"mlnodes":[]}`))
	}))
	t.Cleanup(server.Close)
	observer := newPoCPhaseObserver(t, server, nil)
	previous := httpguard.AllowPrivate()
	httpguard.SetAllowPrivate(false)
	t.Cleanup(func() { httpguard.SetAllowPrivate(previous) })
	byName := "http://localhost:" + strings.TrimPrefix(server.URL, "http://127.0.0.1:")
	observer.versions.SetCandidates(map[string]string{"miner-by-ip": server.URL, "miner-by-name": byName})

	observer.versions.Poll(t.Context())

	if got := hits.Load(); got != 0 {
		t.Fatalf("hits on the private miner = %d, want 0", got)
	}
}

// Test flow:
//  1. Start versions stubs that answer a body past the size limit, and a body malformed in the shape that makes goccy read past its buffer.
//  2. Poll a cache against each.
//  3. Assert the node reads as not validation-capable and the poll returns: a miner's reply cannot grow the gateway's memory without bound or crash its poller.
func TestVersionsCache_AnOversizedOrMalformedReply_FailsClosed(t *testing.T) {
	oversized := `{"mlnodes":[{"node_id":"node-1","poc_validation_inference":true}],"padding":"` + strings.Repeat("x", versionsResponseMaxBytes) + `"}`
	malformed := `{"mlnodes":1,"` + "\\0\x05\\"
	for name, body := range map[string]string{"oversized": oversized, "malformed": malformed} {
		t.Run(name, func(t *testing.T) {
			server := newVersionsStub(t, http.StatusOK, body)
			cache := NewVersionsCache(server.Client(), time.Minute, newFakeClock(time.Unix(0, 0)).Now)
			cache.SetCandidates(map[string]string{"miner-1": server.URL})

			cache.Poll(context.Background())

			if cache.IsNodeValidationCapable("miner-1", "node-1") {
				t.Fatalf("a %s reply should fail closed", name)
			}
		})
	}
}

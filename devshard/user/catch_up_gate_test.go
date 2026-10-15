package user

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"devshard/host"
	"devshard/storage"
)

var errPeerBudgetSpent = errors.New("resource_exhausted: rate limit exceeded")

// peerBudgetRefusingClient counts the requests it is sent and refuses every one, as a host past its peer budget does.
type peerBudgetRefusingClient struct {
	*InProcessClient
	mu    sync.Mutex
	sends int
}

func (c *peerBudgetRefusingClient) Send(context.Context, host.HostRequest, io.Writer, func(*host.HostResponse)) (*host.HostResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sends++
	return nil, errPeerBudgetSpent
}

func (c *peerBudgetRefusingClient) sent() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sends
}

// singleChunkAttempt stops a catch-up from resending a refused chunk in place, so a test counts the runs that reach the host.
func singleChunkAttempt(t *testing.T) {
	t.Helper()
	attempts := catchUpChunkAttempts
	catchUpChunkAttempts = 1
	t.Cleanup(func() { catchUpChunkAttempts = attempts })
}

// Test flow:
//  1. Compose more than one chunk of diffs and make host 0 refuse every request.
//  2. Catch host 0 up for a send: it is refused.
//  3. Catch it up again before the refusal's backoff passes, then again after it.
//  4. Assert the second catch-up fails with the host's own refusal without contacting the host, and the third contacts it again: a host spending its peer budget on catch-ups that fail cannot be asked again on every send, each time reading a chunk of diffs from the store.
func TestSession_ACatchUpTheHostRefusedWaitsOutItsBackoff(t *testing.T) {
	session := setupStoredSession(t, storage.NewMemory())
	const n = catchUpChunkSize + 100
	composeEmptyDiffs(t, session, n)
	refusing := &peerBudgetRefusingClient{InProcessClient: session.clients[0].(*InProcessClient)}
	session.clients[0] = refusing
	now := time.Unix(1_800_000_000, 0)
	session.clock = func() time.Time { return now }
	singleChunkAttempt(t)

	_, firstErr := session.catchUpForSend(context.Background(), 0, n)
	sentAfterFirst := refusing.sent()
	_, withinBackoffErr := session.catchUpForSend(context.Background(), 0, n)
	sentWithinBackoff := refusing.sent()
	now = now.Add(catchUpRefusalMaxBackoff)
	_, afterBackoffErr := session.catchUpForSend(context.Background(), 0, n)

	require.ErrorIs(t, firstErr, errPeerBudgetSpent)
	require.Equal(t, 1, sentAfterFirst, "the first catch-up asks the host once and stops at its refusal")
	require.ErrorIs(t, withinBackoffErr, errPeerBudgetSpent, "a catch-up inside the backoff answers with the host's refusal")
	require.Equal(t, sentAfterFirst, sentWithinBackoff, "a catch-up inside the backoff does not contact the host")
	require.ErrorIs(t, afterBackoffErr, errPeerBudgetSpent)
	require.Equal(t, sentAfterFirst+1, refusing.sent(), "once the backoff passed the host is asked again")
}

// Test flow:
//  1. Compose more than one chunk of diffs and make host 0 refuse, then accept again.
//  2. Catch it up while it refuses, wait out the backoff, and catch it up once it accepts.
//  3. Assert the accepted catch-up clears the refusal, so the next send is not held back by it.
func TestSession_ACatchUpThatSucceedsClearsTheRefusal(t *testing.T) {
	session := setupStoredSession(t, storage.NewMemory())
	const n = catchUpChunkSize + 100
	composeEmptyDiffs(t, session, n)
	accepting := session.clients[0]
	refusing := &peerBudgetRefusingClient{InProcessClient: accepting.(*InProcessClient)}
	session.clients[0] = refusing
	now := time.Unix(1_800_000_000, 0)
	session.clock = func() time.Time { return now }
	singleChunkAttempt(t)

	_, refusedErr := session.catchUpForSend(context.Background(), 0, n)
	session.clients[0] = accepting
	now = now.Add(catchUpRefusalMaxBackoff)
	_, acceptedErr := session.catchUpForSend(context.Background(), 0, n)

	require.ErrorIs(t, refusedErr, errPeerBudgetSpent)
	require.NoError(t, acceptedErr)
	require.NoError(t, session.hostCatchUpRefusal(0).refusal(now), "a catch-up the host accepted leaves no refusal behind")
}

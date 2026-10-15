package user

import (
	"context"
	"sync"
	"time"
)

const (
	catchUpRefusalFirstBackoff = time.Second
	catchUpRefusalMaxBackoff   = 30 * time.Second
)

// catchUpGate remembers a host's last catch-up refusal, so a host that keeps refusing is not asked again on every
// send and every timeout vote, each time with a chunk of diffs read from the store.
type catchUpGate struct {
	mu      sync.Mutex
	refused error
	retryAt time.Time
	backoff time.Duration
}

// refusal is the host's last refusal while its backoff runs, else nil.
func (g *catchUpGate) refusal(now time.Time) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.refused == nil || !now.Before(g.retryAt) {
		return nil
	}
	return g.refused
}

// record keeps a refusal and doubles its backoff; a success clears both. A catch-up given up by its own context
// says nothing about the host.
func (g *catchUpGate) record(ctx context.Context, err error, now time.Time) {
	if ctx.Err() != nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if err == nil {
		g.refused, g.retryAt, g.backoff = nil, time.Time{}, 0
		return
	}
	g.backoff = min(max(2*g.backoff, catchUpRefusalFirstBackoff), catchUpRefusalMaxBackoff)
	g.refused, g.retryAt = err, now.Add(g.backoff)
}

// hostCatchUpRefusal is the one refusal memory of a host index.
func (s *Session) hostCatchUpRefusal(hostIdx int) *catchUpGate {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.catchUpRefusals == nil {
		s.catchUpRefusals = make(map[int]*catchUpGate)
	}
	gate, ok := s.catchUpRefusals[hostIdx]
	if !ok {
		gate = &catchUpGate{}
		s.catchUpRefusals[hostIdx] = gate
	}
	return gate
}

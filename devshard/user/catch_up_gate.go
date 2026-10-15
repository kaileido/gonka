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

// catchUpGate serializes chunked catch-up to one host and remembers the host's last refusal, so a host that
// keeps refusing is not asked again on every send, each time with a chunk of diffs read from the store.
type catchUpGate struct {
	sync.Mutex
	refusalMu sync.Mutex
	refused   error
	retryAt   time.Time
	backoff   time.Duration
}

// refusal is the host's last refusal while its backoff runs, else nil.
func (g *catchUpGate) refusal(now time.Time) error {
	g.refusalMu.Lock()
	defer g.refusalMu.Unlock()
	if g.refused == nil || !now.Before(g.retryAt) {
		return nil
	}
	return g.refused
}

// record keeps a refusal and doubles its backoff; a success clears both. A catch-up the caller itself gave up says nothing about the host.
func (g *catchUpGate) record(ctx context.Context, err error, now time.Time) {
	if ctx.Err() != nil {
		return
	}
	g.refusalMu.Lock()
	defer g.refusalMu.Unlock()
	if err == nil {
		g.refused, g.retryAt, g.backoff = nil, time.Time{}, 0
		return
	}
	g.backoff = min(max(2*g.backoff, catchUpRefusalFirstBackoff), catchUpRefusalMaxBackoff)
	g.refused, g.retryAt = err, now.Add(g.backoff)
}

// hostCatchUpGate is the one gate of a host index.
func (s *Session) hostCatchUpGate(hostIdx int) *catchUpGate {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hostCatchUp == nil {
		s.hostCatchUp = make(map[int]*catchUpGate)
	}
	gate, ok := s.hostCatchUp[hostIdx]
	if !ok {
		gate = &catchUpGate{}
		s.hostCatchUp[hostIdx] = gate
	}
	return gate
}

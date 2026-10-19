// Package scheduler picks the escrow, host, and nonce that serve a chat-completions request.
package scheduler

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"devshard/cmd/gateway/config"
)

// idleDispatcherGrace is how long an escrow's actor stays alive with an empty queue. See routing.md, "Idle dispatchers are reaped".
const idleDispatcherGrace = 5 * time.Minute

// Deps wires the runtime facts routing reads; Observer, Now, OnEscrowExhausted, OnReserveTaken and OnMoneyShort are optional.
type Deps struct {
	Escrows           escrowSource
	Capacity          escrowWeights
	Limiter           hostLimiter
	Perf              hostHealth
	Snapshots         snapshotSource
	Config            *config.Holder
	Observer          dispatchObserver
	Now               func() time.Time
	OnEscrowExhausted func(escrowID string, reason ExhaustionReason)
	OnReserveTaken    func(escrowID string)
	OnMoneyShort      func(model string)
}

// Scheduler owns one actor per escrow. See routing.md, "Picking an escrow".
type Scheduler struct {
	escrows           escrowSource
	capacity          escrowWeights
	limiter           hostLimiter
	perf              hostHealth
	snapshots         snapshotSource
	settings          *config.Holder
	observer          dispatchObserver
	now               func() time.Time
	newTimer          func(time.Duration) (<-chan time.Time, func())
	onEscrowExhausted func(escrowID string, reason ExhaustionReason)
	onReserveTaken    func(escrowID string)
	onMoneyShort      func(model string)

	tieBreak atomic.Int64

	registryMu  sync.Mutex
	dispatchers map[string]*dispatcher
	superseded  map[*dispatcher]struct{}
	stopped     bool

	blocksMu     sync.RWMutex
	blockedHosts map[string]map[string]bool
	replays      replayCredit
}

// NewScheduler refuses a dependency set it cannot route with, rather than panicking on the first request.
func NewScheduler(deps Deps) (*Scheduler, error) {
	switch {
	case deps.Config == nil:
		return nil, errors.New("scheduler: Config is required")
	case deps.Escrows == nil:
		return nil, errors.New("scheduler: Escrows is required")
	case deps.Capacity == nil:
		return nil, errors.New("scheduler: Capacity is required")
	case deps.Limiter == nil:
		return nil, errors.New("scheduler: Limiter is required")
	case deps.Perf == nil:
		return nil, errors.New("scheduler: Perf is required")
	case deps.Snapshots == nil:
		return nil, errors.New("scheduler: Snapshots is required")
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	return &Scheduler{
		escrows:           deps.Escrows,
		capacity:          deps.Capacity,
		limiter:           deps.Limiter,
		perf:              deps.Perf,
		snapshots:         deps.Snapshots,
		settings:          deps.Config,
		observer:          deps.Observer,
		now:               deps.Now,
		onEscrowExhausted: deps.OnEscrowExhausted,
		onReserveTaken:    deps.OnReserveTaken,
		onMoneyShort:      deps.OnMoneyShort,
		dispatchers:       map[string]*dispatcher{},
		blockedHosts:      map[string]map[string]bool{},
	}, nil
}

// HostDiverged reports whether the participant still had its catch-up replay; the last one blocks it.
func (s *Scheduler) HostDiverged(escrowID, participant string, at time.Time) bool {
	if s.replays.spend(escrowID, participant, at) {
		return true
	}
	s.BlockHost(escrowID, participant)
	return false
}

// HostServed returns the replay to a participant whose later send the group accepted.
func (s *Scheduler) HostServed(escrowID, participant string, sentAt time.Time) {
	s.replays.restore(escrowID, participant, sentAt)
}

// BlockHost bars a participant from one escrow for as long as the escrow's dispatcher lives. See routing.md.
func (s *Scheduler) BlockHost(escrowID, participant string) {
	s.blocksMu.Lock()
	defer s.blocksMu.Unlock()
	blocked := s.blockedHosts[escrowID]
	if blocked == nil {
		blocked = map[string]bool{}
		s.blockedHosts[escrowID] = blocked
	}
	blocked[participant] = true
}

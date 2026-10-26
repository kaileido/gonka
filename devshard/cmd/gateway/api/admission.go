package api

import (
	"errors"
	"sync/atomic"
	"time"

	"devshard/cmd/gateway/chain"
	"devshard/cmd/gateway/config"
)

// admission is the pre-queue chain check. See README.md, "What the boundary hands the engine".
func admission(snapshot chain.PhaseSnapshot, modes config.Modes, now time.Time, maxAgeSeconds int64) error {
	if !modes.BlocksRequests(snapshot) {
		if stale := staleness(snapshot, now, maxAgeSeconds); stale != nil {
			return stale
		}
		return nil
	}
	return &BlockedError{
		Reason:     snapshot.BlockReason,
		Phase:      snapshot.EpochPhase,
		Confirming: snapshot.ConfirmationPoCPhase,
	}
}

func blockedOnlyByPoC(refusal error) bool {
	var blocked *BlockedError
	if !errors.As(refusal, &blocked) {
		return false
	}
	return blocked.Reason == chain.BlockReasonPoC || blocked.Reason == chain.BlockReasonConfirmationPoC
}

func staleness(snapshot chain.PhaseSnapshot, now time.Time, maxAgeSeconds int64) error {
	if maxAgeSeconds <= 0 {
		return nil
	}
	limit := time.Duration(maxAgeSeconds) * time.Second
	if snapshot.LastHealthyAt.IsZero() {
		return &ChainStaleError{Age: limit}
	}
	if age := now.Sub(snapshot.LastHealthyAt); age >= limit {
		return &ChainStaleError{Age: age}
	}
	return nil
}

type limiterSlot struct {
	holders atomic.Int32
	release func()
}

func newLimiterSlot(release func()) *limiterSlot {
	slot := &limiterSlot{release: release}
	slot.holders.Store(1)
	return slot
}

func (s *limiterSlot) share() func() {
	s.holders.Add(1)
	return s.done
}

func (s *limiterSlot) done() {
	if s.holders.Add(-1) == 0 {
		s.release()
	}
}

package limits

import (
	"math"
	"time"
)

func cutoffState(state *hostState, now time.Time) CutoffState {
	switch {
	case now.Before(state.openUntil):
		return CutoffOpen
	case state.halfOpen || !state.openUntil.IsZero():
		return CutoffHalfOpen
	}
	return CutoffClosed
}

// ClearQuarantine reopens one participant's cutoffs. See capacity.md, "The participant limiter: IOCW".
func (l *ParticipantLimiter) ClearQuarantine(participant string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	cleared := false
	for tracked, state := range l.states {
		if tracked.participant != participant {
			continue
		}
		state.input.reopen(state.bounds.Input.Initial)
		state.output.reopen(state.bounds.Output.Initial)
		state.consecutiveCutoffFaults = 0
		state.openUntil = time.Time{}
		state.backoffCount = 0
		state.halfOpen = false
		cleared = true
	}
	return cleared
}

func cutoffReason(halfOpen bool) string {
	if halfOpen {
		return cutoffReasonProbeFailed
	}
	return cutoffReasonConsecutiveFaults
}

// OnResult applies one finished attempt to the host's windows and its cutoff.
func (l *ParticipantLimiter) OnResult(result Result) {
	answer := responseFor(result.Verdict)
	if answer.inert() {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	state := l.stateLocked(key{participant: result.Participant, model: result.Model})
	now := l.now()
	state.lastUsed = now

	narrowingTier, blamed := answer.tier, answer.dimension
	if narrowingTier == tierNone && result.Verdict == Success {
		if congested := result.Pressure.congestedDimension(l.cfg.Slack); congested != dimensionNone {
			narrowingTier, blamed = tierSoft, congested
		} else {
			l.growLocked(state, result.Carried)
		}
	}
	if by := l.cfg.Factors.narrowingFor(narrowingTier, blamed); by.moves() {
		l.narrowLocked(state, by)
	}
	l.applyBreakerLocked(state, answer.breaker, result.Participant, result.Model, now)
}

// CountRefusalIfIdle counts a refusal towards the cut-off when the host carries nothing else of ours for this model. See README.md, "What blames which window".
func (l *ParticipantLimiter) CountRefusalIfIdle(participant, model string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	state := l.stateLocked(key{participant: participant, model: model})
	now := l.now()
	state.lastUsed = now
	if !state.idle() {
		return
	}
	l.applyBreakerLocked(state, breakerCounts, participant, model, now)
}

// growLocked widens each window whose peak earned it. See README.md, "Additive increase".
func (l *ParticipantLimiter) growLocked(state *hostState, carried TokenCost) {
	if float64(state.input.peak) >= state.input.tokens/2 {
		state.input.tokens = grownBy(state.input.tokens, carried.Input, float64(state.bounds.Input.Step))
		state.input.peak = state.input.inflight
	}
	if float64(state.output.peak) >= state.output.tokens/2 {
		state.output.tokens = grownBy(state.output.tokens, carried.Output, float64(state.bounds.Output.Step))
		state.output.peak = state.output.inflight
	}
}

// narrowLocked applies one signal's factors to the two windows.
func (l *ParticipantLimiter) narrowLocked(state *hostState, by narrowing) {
	narrowWindow(&state.input, by.input, state.bounds.Input.Min)
	narrowWindow(&state.output, by.output, state.bounds.Output.Min)
}

// narrowWindow restarts the peak of a window it actually moved. See README.md, "Additive increase".
func narrowWindow(w *window, factor float64, floor int64) {
	if factor == 1 {
		return
	}
	w.tokens = narrowedTo(w.tokens, factor, float64(floor))
	w.peak = w.inflight
}

func (l *ParticipantLimiter) applyBreakerLocked(state *hostState, effect breakerEffect, participant, model string, now time.Time) {
	switch effect {
	case breakerRecovers:
		state.consecutiveCutoffFaults = 0
		if !state.halfOpen {
			return
		}
		state.halfOpen = false
		state.openUntil = time.Time{}
		if state.backoffCount > 0 {
			state.backoffCount--
		}
		if l.narrator != nil {
			l.narrator.HostCutOffLifted(participant, model, state.backoffCount)
		}
	case breakerCounts:
		if now.Before(state.openUntil) {
			return
		}
		state.consecutiveCutoffFaults++
		if state.consecutiveCutoffFaults < int(l.cfg.AfterFailures) && !state.halfOpen {
			return
		}
		capped := min(time.Duration(float64(l.cfg.BaseOpen)*math.Pow(1.6, float64(state.backoffCount))), l.cfg.MaxOpen)
		state.openUntil = now.Add(capped + l.jitter(capped))
		if capped < l.cfg.MaxOpen {
			state.backoffCount++
		}
		reason := cutoffReason(state.halfOpen)
		state.halfOpen = false
		state.consecutiveCutoffFaults = 0
		if l.narrator != nil {
			l.narrator.HostCutOff(participant, model, reason, state.backoffCount, state.openUntil.Sub(now))
		}
	}
}

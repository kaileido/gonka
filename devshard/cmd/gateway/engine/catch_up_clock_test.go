package engine

import (
	"testing"
	"time"
)

// Test flow:
//  1. Plan the deadlines of an attempt still waiting on its catch-up and of one whose request has left.
//  2. Assert the waiting attempt's clock starts at the latest a minute after its launch, and the sent one's at its send.
func TestACatchUpHoldsAnAttemptsClockForAMinuteAtMost(t *testing.T) {
	waiting := &liveAttempt{nonce: 600, participant: "host-0", launchTime: testEpoch, cancel: func() {}}
	sent := &liveAttempt{nonce: 601, participant: "host-1", launchTime: testEpoch, sendTime: testEpoch.Add(5 * time.Second), cancel: func() {}}
	coordinator := pausedCoordinator(newRaceFixture(settledPolicy(), 2), 2, waiting, sent)

	plan := coordinator.plan()

	if got, want := plan.Attempts[0].ClockStart, testEpoch.Add(maxCatchUpBeforeSend); !got.Equal(want) {
		t.Fatalf("waiting attempt's clock starts at %v, want %v", got, want)
	}
	if got, want := plan.Attempts[1].ClockStart, testEpoch.Add(5*time.Second); !got.Equal(want) {
		t.Fatalf("sent attempt's clock starts at %v, want %v", got, want)
	}
}

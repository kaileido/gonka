package loadtest

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Test flow:
// 1. Record four runs of one known_bug check in a history file: XPASS, XFAIL, XPASS, XPASS.
// 2. Require the last run to count 2 consecutive XPASS runs and its details to ask for 5 before dropping the marker.
// 3. Record the last run again and require the count to stay 2.
// 4. Record three more XPASS runs and require 5, with details that say to drop the marker.
// 5. Record a run older than all of them and require its count to cover only runs up to it.
func TestKnownBugHistoryAsksForFiveConsecutiveXPass(t *testing.T) {
	path := filepath.Join(t.TempDir(), knownBugHistoryFile)
	key := knownBugKey{Scenario: "prod-like-15m", Gateway: "devshardctl"}
	check := func(status string) []AssertionResult {
		return []AssertionResult{
			{Name: "invariants.error-rate-and-latency.timeout-vote-fanout.error_rate", Case: "timeout-vote-fanout", Status: status, Passed: true, Details: "known_bug #1988"},
			{Name: "requests.error_rate", Passed: true},
		}
	}
	record := func(offset time.Duration, status string) []AssertionResult {
		t.Helper()
		run := filepath.Join("runs", offset.String())
		marked, err := recordKnownBugRuns(path, key, run, at(offset), check(status))
		require.NoError(t, err)
		return marked
	}

	record(time.Hour, "XPASS")
	record(2*time.Hour, "XFAIL")
	record(3*time.Hour, "XPASS")
	marked := record(4*time.Hour, "XPASS")
	require.Equal(t, "known_bug #1988; XPASS 2 runs in a row of the 5 needed to drop the marker", marked[0].Details)
	require.Equal(t, check("XPASS")[1], marked[1])

	marked = record(4*time.Hour, "XPASS")
	require.Contains(t, marked[0].Details, "XPASS 2 runs in a row")

	record(5*time.Hour, "XPASS")
	record(6*time.Hour, "XPASS")
	marked = record(7*time.Hour, "XPASS")
	require.Equal(t, "known_bug #1988; XPASS 5 runs in a row: drop the marker", marked[0].Details)

	marked = record(0, "XPASS")
	require.Contains(t, marked[0].Details, "XPASS 1 runs in a row")
}

package loadtest

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"time"
)

const (
	// knownBugXPassRuns is how many runs in a row a known_bug check must pass before the report
	// says to drop its marker: timeout-vote-fanout's bug reproduced in only 1 of 3 runs of identical code.
	knownBugXPassRuns = 5
	// knownBugHistoryFile sits beside the run directories by default, so repeats of a scenario share it.
	knownBugHistoryFile = "known-bug-history.json"
)

// knownBugKey names the runs whose known_bug outcomes form one history.
type knownBugKey struct {
	Scenario string `json:"scenario"`
	Gateway  string `json:"gateway"`
}

// knownBugRun is one run's outcome of one known_bug check.
type knownBugRun struct {
	Run     string    `json:"run"`
	Started time.Time `json:"started"`
	Status  string    `json:"status"` // XPASS or XFAIL
}

// knownBugHistory is the JSON file: outcomes by "scenario|gateway|check", one per run start.
type knownBugHistory struct {
	Checks map[string][]knownBugRun `json:"checks"`
}

// defaultKnownBugHistory is the history file beside run directory dir.
func defaultKnownBugHistory(dir string) string {
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	return filepath.Join(filepath.Dir(dir), knownBugHistoryFile)
}

// recordKnownBugRuns adds this run's XPASS and XFAIL checks to the history at path, replacing an
// earlier record of the same run start, and returns checks with each XPASS check's details naming
// how many runs in a row, up to this one, it passed. One writer at a time: runs on a VM are serial.
func recordKnownBugRuns(path string, key knownBugKey, run string, started time.Time, checks []AssertionResult) ([]AssertionResult, error) {
	history := knownBugHistory{Checks: map[string][]knownBugRun{}}
	body, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return checks, err
	default:
		if err := json.Unmarshal(body, &history); err != nil {
			return checks, fmt.Errorf("%s: %w", path, err)
		}
		if history.Checks == nil {
			history.Checks = map[string][]knownBugRun{}
		}
	}
	marked := append([]AssertionResult(nil), checks...)
	changed := false
	for i, check := range marked {
		if check.Status != "XPASS" && check.Status != "XFAIL" {
			continue
		}
		name := key.Scenario + "|" + key.Gateway + "|" + check.Name
		runs := slices.DeleteFunc(slices.Clone(history.Checks[name]), func(r knownBugRun) bool { return r.Started.Equal(started) })
		runs = append(runs, knownBugRun{Run: run, Started: started, Status: check.Status})
		sort.Slice(runs, func(a, b int) bool { return runs[a].Started.Before(runs[b].Started) })
		history.Checks[name], changed = runs, true
		if check.Status != "XPASS" {
			continue
		}
		inARow := 0
		for j := len(runs) - 1; j >= 0; j-- {
			if runs[j].Started.After(started) {
				continue
			}
			if runs[j].Status != "XPASS" {
				break
			}
			inARow++
		}
		if inARow >= knownBugXPassRuns {
			marked[i].Details = fmt.Sprintf("%s; XPASS %d runs in a row: drop the marker", check.Details, inARow)
		} else {
			marked[i].Details = fmt.Sprintf("%s; XPASS %d runs in a row of the %d needed to drop the marker", check.Details, inARow, knownBugXPassRuns)
		}
	}
	if !changed {
		return marked, nil
	}
	body, err = json.MarshalIndent(history, "", "  ")
	if err != nil {
		return checks, err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(body, '\n'), 0o644); err != nil {
		return checks, err
	}
	return marked, os.Rename(tmp, path)
}

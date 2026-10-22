package engine

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

// Test flow:
//  1. For each host script in the table (content then [DONE], content on a finished nonce, both, and neither), run a one-host race through the engine.
//  2. Assert the error Run returns, the status that error maps to, whether the race succeeded, and the winner's terminal.
//  3. Assert a winner whose nonce is unfinished is still planned a posted timeout vote.
func TestAnAnswerCutShortFailsTheRace(t *testing.T) {
	const sseDone = "data: [DONE]\n\n"
	testCases := []struct {
		name          string
		chunks        []string
		finished      bool
		wantErr       error
		wantStatus    int
		wantSucceeded bool
		wantTerminal  Terminal
	}{
		{name: "a finished answer", chunks: []string{roleEvent, contentEvent("whole"), sseDone}, finished: true, wantStatus: http.StatusOK, wantSucceeded: true, wantTerminal: TerminalWon},
		{name: "a terminated answer on an unfinished nonce", chunks: []string{roleEvent, contentEvent("whole"), sseDone}, wantStatus: http.StatusOK, wantSucceeded: true, wantTerminal: TerminalWon},
		{name: "an unterminated answer on a finished nonce", chunks: []string{roleEvent, contentEvent("whole")}, finished: true, wantStatus: http.StatusOK, wantSucceeded: true, wantTerminal: TerminalWon},
		{name: "an answer cut short", chunks: []string{roleEvent, contentEvent("half")}, wantErr: ErrAnswerTruncated, wantStatus: http.StatusBadGateway, wantTerminal: TerminalStreamTruncated},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			sim := newSimulator(t, settledPolicy(), 1, qwenModel)
			sim.host(10, 0, "host-0", &hostScript{
				receipt:   true,
				chunks:    testCase.chunks,
				confirmed: true,
				finished:  testCase.finished,
			})

			_, err := sim.run(context.Background())
			reported := sim.reported(t)
			sim.settleAll()

			if !errors.Is(err, testCase.wantErr) || (testCase.wantErr == nil && err != nil) {
				t.Fatalf("Run() error = %v, want %v", err, testCase.wantErr)
			}
			if status := StatusForError(err); status != testCase.wantStatus {
				t.Errorf("StatusForError(%v) = %d, want %d", err, status, testCase.wantStatus)
			}
			if reported.Succeeded != testCase.wantSucceeded {
				t.Errorf("Succeeded = %t, want %t", reported.Succeeded, testCase.wantSucceeded)
			}
			if len(reported.Attempts) != 1 {
				t.Fatalf("attempts = %d, want 1", len(reported.Attempts))
			}
			winner := reported.Attempts[0]
			if !reported.IsWinner(winner) {
				t.Fatalf("the only attempt %d is not the winner %d", winner.Nonce, reported.WinnerNonce)
			}
			if winner.Terminal != testCase.wantTerminal {
				t.Errorf("winner terminal = %v, want %v", winner.Terminal, testCase.wantTerminal)
			}
			if testCase.finished {
				return
			}
			plan := reported.TimeoutPlan()
			if len(plan) != 1 || plan[0].Nonce != winner.Nonce || !plan[0].Post {
				t.Errorf("timeout plan = %+v, want one posted vote for nonce %d", plan, winner.Nonce)
			}
		})
	}
}

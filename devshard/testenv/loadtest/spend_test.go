package loadtest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"devshard/testenv/config"
	"devshard/testenv/mockchain/ledgerface"
)

func spendFixtureLedger() *chainLedger {
	return &chainLedger{
		SettleTxs: []ledgerface.TxView{
			settleTx(1, 10, 40, "r40-early", 5, ledgerface.SettleHostStatView{SlotID: 0, Cost: 100}),
			settleTx(2, 10, 42, "r42", 10, ledgerface.SettleHostStatView{SlotID: 0, Cost: 300}, ledgerface.SettleHostStatView{SlotID: 1, Cost: 200}),
			settleTx(3, 11, 8, "r8", 4, ledgerface.SettleHostStatView{SlotID: 0, Cost: 96}),
			{Seq: 4, Type: "settle"},
		},
		Escrows: []ledgerface.EscrowView{
			{ID: 12, ModelID: "m-a", Amount: 3000},
			{ID: 10, ModelID: "m-a", Amount: 1000, Settled: true},
			{ID: 11, ModelID: "m-b", Amount: 2000, Settled: true},
		},
	}
}

func TestBuildSpendPerEscrowFromChainLedger(t *testing.T) {
	// Test flow:
	// 1. Build the spend block from a fixture ledger with two settled escrows, one settled twice, and one unsettled escrow.
	// 2. Check that escrows are sorted by id and that the latest settle of each escrow sets its spend and max nonce.
	// 3. Check that inferences count only finished, validated and challenged records.
	// 4. Check that the unsettled escrow and the escrow with no inference list give reasons instead of zeros.
	inferences := map[string]map[string]DebugInference{
		"10": {"1": {Status: "finished"}, "2": {Status: "validated"}, "3": {Status: "challenged"}, "4": {Status: "failed"}, "5": {Status: "started"}},
		"12": {"1": {Status: "finished"}},
	}
	spend := buildSpend(spendFixtureLedger(), inferences, "", 10)
	if spend.Unavailable != "" || len(spend.Escrows) != 3 {
		t.Fatalf("spend = %+v", spend)
	}
	first, second, third := spend.Escrows[0], spend.Escrows[1], spend.Escrows[2]
	if first.EscrowID != "10" || second.EscrowID != "11" || third.EscrowID != "12" {
		t.Fatalf("escrow order = %s %s %s", first.EscrowID, second.EscrowID, third.EscrowID)
	}
	if !first.Settled || first.DepositNgonka != 1000 || first.SettleTxs != 2 || *first.SpentNgonka != 510 || *first.HostCostNgonka != 500 || *first.FeesNgonka != 10 || *first.MaxNonce != 42 || *first.InferencesCompleted != 3 {
		t.Fatalf("escrow 10 = %+v", first)
	}
	if *second.SpentNgonka != 100 || *second.MaxNonce != 8 || second.InferencesCompleted != nil || second.Unavailable["inferences_completed"] == "" {
		t.Fatalf("escrow 11 = %+v", second)
	}
	if third.Settled || third.SpentNgonka != nil || third.MaxNonce != nil || third.Unavailable["spent_ngonka"] == "" || third.Unavailable["max_nonce"] == "" || *third.InferencesCompleted != 1 {
		t.Fatalf("escrow 12 = %+v", third)
	}
}

func TestBuildSpendTotals(t *testing.T) {
	// Test flow:
	// 1. Build the spend block from the fixture ledger with 10 completed requests.
	// 2. Check the totals over the settled escrows and the per-request ratios.
	// 3. Check that the totals name the unsettled escrow as left out.
	spend := buildSpend(spendFixtureLedger(), map[string]map[string]DebugInference{}, "", 10)
	totals := spend.Totals
	if totals.Escrows != 3 || totals.SettledEscrows != 2 || *totals.SpentNgonka != 610 || *totals.NoncesUsed != 50 {
		t.Fatalf("totals = %+v", totals)
	}
	if *totals.SpentGNK != 610e-9 || *totals.NgonkaPerCompletedRequest != 61 || *totals.NoncesPerCompletedRequest != 5 {
		t.Fatalf("ratios = %+v", totals)
	}
	if totals.Partial["spent_ngonka"] != "escrows 12 not settled by the end of the run: the chain paid nothing out of them yet" {
		t.Fatalf("partial = %+v", totals.Partial)
	}
}

func TestBuildSpendUnavailable(t *testing.T) {
	// Test flow:
	// 1. Build the spend block with no ledger and a ledger error, and check that it names the reason and writes no totals.
	// 2. Build it from a ledger with no settled escrow and no completed requests, and check that every total gives a reason.
	missing := buildSpend(nil, nil, "connection refused", 10)
	if missing.Unavailable != "mock-chain ledger: connection refused" || missing.Totals.SpentNgonka != nil || len(missing.Escrows) != 0 {
		t.Fatalf("missing = %+v", missing)
	}
	unsettled := buildSpend(&chainLedger{Escrows: []ledgerface.EscrowView{{ID: 5, Amount: 100}}}, nil, "", 0)
	totals := unsettled.Totals
	if totals.SpentNgonka != nil || totals.NoncesUsed != nil || totals.NgonkaPerCompletedRequest != nil || totals.NoncesPerCompletedRequest != nil {
		t.Fatalf("totals = %+v", totals)
	}
	for _, field := range []string{"spent_ngonka", "nonces_used", "ngonka_per_completed_request", "nonces_per_completed_request"} {
		if totals.Unavailable[field] == "" {
			t.Fatalf("%s has no reason: %+v", field, totals.Unavailable)
		}
	}
	if unsettled.Escrows[0].Unavailable["inferences_completed"] == "" {
		t.Fatalf("escrow = %+v", unsettled.Escrows[0])
	}
}

func TestSpendLedgerPrefersInvariantRead(t *testing.T) {
	// Test flow:
	// 1. Give run inputs both an invariant end state and a separate chain ledger read, and check that the invariant read wins.
	// 2. Give run inputs only the separate read, and check that it is used without inference lists.
	// 3. Give run inputs neither, and check that the reason says the run did not save it.
	invariantLedger := spendFixtureLedger()
	inputs := RunInputs{
		Invariants:  &invariantEndState{SettleTxs: invariantLedger.SettleTxs, Escrows: invariantLedger.Escrows, Inferences: map[string]map[string]DebugInference{"10": {}}},
		ChainLedger: &chainLedger{Escrows: []ledgerface.EscrowView{{ID: 99}}},
	}
	ledger, inferences, reason := spendSources(inputs)
	if reason != "" || len(ledger.Escrows) != 3 || inferences == nil {
		t.Fatalf("invariant read: ledger=%+v inferences=%v reason=%q", ledger, inferences, reason)
	}
	inputs.Invariants = nil
	ledger, inferences, reason = spendSources(inputs)
	if reason != "" || len(ledger.Escrows) != 1 || inferences != nil {
		t.Fatalf("separate read: ledger=%+v inferences=%v reason=%q", ledger, inferences, reason)
	}
	_, _, reason = spendSources(RunInputs{})
	if reason != notSaved {
		t.Fatalf("reason = %q", reason)
	}
}

func TestWriteRunSummarySpendForBothGateways(t *testing.T) {
	// Test flow:
	// 1. Write run-summary.json for the same fixture ledger under each gateway driver.
	// 2. Read each file back and check that the spend block carries the same chain totals for both.
	for _, driver := range []config.GatewayDriver{config.GatewayDriverDevshardctl, config.GatewayDriverGateway} {
		dir := t.TempDir()
		run := runReport{
			summary: Summary{Scenario: "s", StartedAt: time.Unix(0, 0).UTC(), Duration: time.Minute, Requests: 10, Completed: 10},
			inputs:  RunInputs{Gateway: driver, ChainLedger: spendFixtureLedger()},
		}
		if _, err := writeRunSummary(dir, run); err != nil {
			t.Fatal(err)
		}
		body, err := os.ReadFile(filepath.Join(dir, runSummaryFileName))
		if err != nil {
			t.Fatal(err)
		}
		var decoded struct {
			Spend struct {
				Source string `json:"source"`
				Totals struct {
					SpentNgonka uint64 `json:"spent_ngonka"`
				} `json:"totals"`
				Escrows []json.RawMessage `json:"escrows"`
			} `json:"spend"`
		}
		if err := json.Unmarshal(body, &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded.Spend.Source == "" || decoded.Spend.Totals.SpentNgonka != 610 || len(decoded.Spend.Escrows) != 3 {
			t.Fatalf("%s spend = %+v", driver, decoded.Spend)
		}
	}
}

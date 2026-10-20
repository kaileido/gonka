package loadtest

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"devshard/testenv/mockchain/ledgerface"
)

const (
	ngonkaPerGNK = 1e9
	spendSource  = "mock-chain /testenv/escrows and /testenv/txs?type=settle (spent = last settle's sum(host_stats.cost) + fees); inferences_completed from each escrow's /v1/debug/inferences"
)

// chainLedger is the mock-chain ledger read after drain by a run whose invariants did not read it.
type chainLedger struct {
	SettleTxs []ledgerface.TxView     `json:"settle_txs"`
	Escrows   []ledgerface.EscrowView `json:"escrows"`
	Error     string                  `json:"error,omitempty"`
}

// RunSummarySpend is what the escrows paid out by the end of the run, as the mock chain records it,
// so either gateway is measured alike. Unavailable maps a nil field to the reason it is missing.
type RunSummarySpend struct {
	Source      string        `json:"source"`
	Unavailable string        `json:"unavailable,omitempty"`
	Escrows     []EscrowSpend `json:"escrows"`
	Totals      SpendTotals   `json:"totals"`
}

// EscrowSpend is one escrow on the mock chain; spend and nonce come from its latest settle tx.
type EscrowSpend struct {
	EscrowID            string            `json:"escrow_id"`
	Model               string            `json:"model"`
	DepositNgonka       uint64            `json:"deposit_ngonka"`
	Settled             bool              `json:"settled"`
	SettleTxs           int               `json:"settle_txs"`
	SpentNgonka         *uint64           `json:"spent_ngonka"`
	HostCostNgonka      *uint64           `json:"host_cost_ngonka"`
	FeesNgonka          *uint64           `json:"fees_ngonka"`
	MaxNonce            *uint64           `json:"max_nonce"`
	InferencesCompleted *int              `json:"inferences_completed"`
	Unavailable         map[string]string `json:"unavailable,omitempty"`
}

// SpendTotals sums the settled escrows; Partial names the escrows a present total leaves out.
type SpendTotals struct {
	Escrows                   int               `json:"escrows"`
	SettledEscrows            int               `json:"settled_escrows"`
	CompletedRequests         int               `json:"completed_requests"`
	SpentNgonka               *uint64           `json:"spent_ngonka"`
	SpentGNK                  *float64          `json:"spent_gnk"`
	NoncesUsed                *uint64           `json:"nonces_used"`
	NgonkaPerCompletedRequest *float64          `json:"ngonka_per_completed_request"`
	NoncesPerCompletedRequest *float64          `json:"nonces_per_completed_request"`
	Partial                   map[string]string `json:"partial,omitempty"`
	Unavailable               map[string]string `json:"unavailable,omitempty"`
}

// spendSources returns the chain ledger and inference lists a run saved; the invariant read wins
// because it also carries the inference lists.
func spendSources(inputs RunInputs) (*chainLedger, map[string]map[string]DebugInference, string) {
	if end := inputs.Invariants; end != nil && end.ChainError != notSaved {
		return &chainLedger{SettleTxs: end.SettleTxs, Escrows: end.Escrows}, end.Inferences, end.ChainError
	}
	if inputs.ChainLedger != nil {
		return inputs.ChainLedger, nil, inputs.ChainLedger.Error
	}
	return nil, nil, notSaved
}

// buildSpend builds the spend block; completed is the client's completed request count.
func buildSpend(ledger *chainLedger, inferences map[string]map[string]DebugInference, ledgerErr string, completed int) RunSummarySpend {
	spend := RunSummarySpend{Source: spendSource, Escrows: []EscrowSpend{}, Totals: SpendTotals{CompletedRequests: completed, Unavailable: map[string]string{}}}
	if ledger == nil || ledgerErr != "" {
		spend.Unavailable = "mock-chain ledger: " + cmp.Or(ledgerErr, notSaved)
		for _, field := range []string{"spent_ngonka", "spent_gnk", "nonces_used", "ngonka_per_completed_request", "nonces_per_completed_request"} {
			spend.Totals.Unavailable[field] = spend.Unavailable
		}
		return spend
	}
	latest := map[uint64]*ledgerface.SettleTxView{}
	counts := map[uint64]int{}
	for _, tx := range ledger.SettleTxs {
		if tx.Settle != nil {
			latest[tx.Settle.EscrowID] = tx.Settle
			counts[tx.Settle.EscrowID]++
		}
	}
	var spent, nonces uint64
	var unsettled []string
	settledInferences, settledCounted := 0, true
	for _, escrow := range ledger.Escrows {
		id := strconv.FormatUint(escrow.ID, 10)
		row := EscrowSpend{EscrowID: id, Model: escrow.ModelID, DepositNgonka: escrow.Amount, Settled: escrow.Settled, SettleTxs: counts[escrow.ID], Unavailable: map[string]string{}}
		if settle := latest[escrow.ID]; settle != nil {
			var cost uint64
			for _, stat := range settle.HostStats {
				cost += stat.Cost
			}
			total, fees, nonce := cost+settle.Fees, settle.Fees, settle.Nonce
			row.SpentNgonka, row.HostCostNgonka, row.FeesNgonka, row.MaxNonce = &total, &cost, &fees, &nonce
			spent += total
			nonces += nonce
			spend.Totals.SettledEscrows++
		} else {
			unsettled = append(unsettled, id)
			reason := "not settled by the end of the run: the chain paid nothing out of it yet"
			for _, field := range []string{"spent_ngonka", "host_cost_ngonka", "fees_ngonka", "max_nonce"} {
				row.Unavailable[field] = reason
			}
		}
		if records, ok := inferences[id]; ok {
			count := 0
			for _, inference := range records {
				// The statuses whose cost settlement carries, as escrow-accounting-reconciles counts them.
				switch inference.Status {
				case "finished", "validated", "challenged":
					count++
				}
			}
			row.InferencesCompleted = &count
			if row.SpentNgonka != nil {
				settledInferences += count
			}
		} else {
			if row.SpentNgonka != nil {
				settledCounted = false
			}
			if inferences == nil {
				row.Unavailable["inferences_completed"] = "the chain carries no inference count and the run read no gateway inference lists (invariants off)"
			} else {
				row.Unavailable["inferences_completed"] = "the chain carries no inference count and the gateway served no inference list for this escrow after drain"
			}
		}
		if len(row.Unavailable) == 0 {
			row.Unavailable = nil
		}
		spend.Escrows = append(spend.Escrows, row)
	}
	slices.SortFunc(spend.Escrows, func(a, b EscrowSpend) int {
		x, _ := strconv.ParseUint(a.EscrowID, 10, 64)
		y, _ := strconv.ParseUint(b.EscrowID, 10, 64)
		return cmp.Compare(x, y)
	})
	totals := &spend.Totals
	totals.Escrows = len(spend.Escrows)
	if totals.SettledEscrows == 0 {
		reason := fmt.Sprintf("none of the %d escrows was settled by the end of the run", totals.Escrows)
		for _, field := range []string{"spent_ngonka", "spent_gnk", "nonces_used", "ngonka_per_completed_request", "nonces_per_completed_request"} {
			totals.Unavailable[field] = reason
		}
		return spend
	}
	gnk := float64(spent) / ngonkaPerGNK
	totals.SpentNgonka, totals.SpentGNK, totals.NoncesUsed = &spent, &gnk, &nonces
	// With an escrow unsettled, the client's request count covers spend the totals leave out, so the
	// ratios divide by the settled escrows' completed inferences instead.
	denominator, missing := completed, "no completed requests"
	if len(unsettled) > 0 {
		slices.Sort(unsettled)
		reason := "escrows " + strings.Join(unsettled, ",") + " not settled by the end of the run: the chain paid nothing out of them yet"
		totals.Partial = map[string]string{}
		for _, field := range []string{"spent_ngonka", "spent_gnk", "nonces_used"} {
			totals.Partial[field] = reason
		}
		denominator, missing = settledInferences, "no completed inferences on the settled escrows"
		if !settledCounted {
			denominator, missing = 0, reason+", and a settled escrow has no inference count to divide its spend by"
		}
		if denominator > 0 {
			for _, field := range []string{"ngonka_per_completed_request", "nonces_per_completed_request"} {
				totals.Partial[field] = reason + "; divided by the settled escrows' completed inferences"
			}
		}
	}
	if denominator > 0 {
		perRequest, noncesPerRequest := float64(spent)/float64(denominator), float64(nonces)/float64(denominator)
		totals.NgonkaPerCompletedRequest, totals.NoncesPerCompletedRequest = &perRequest, &noncesPerRequest
	} else {
		totals.Unavailable["ngonka_per_completed_request"] = missing
		totals.Unavailable["nonces_per_completed_request"] = missing
	}
	if len(totals.Unavailable) == 0 {
		totals.Unavailable = nil
	}
	return spend
}

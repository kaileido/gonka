package accounting

import (
	"cmp"
	"slices"

	"devshard/cmd/gateway/filters"
)

type inputCount struct {
	tokens uint64
	source filters.InputTokensSource
}

// PersistedInputCount is the gateway's tokenizer count of one nonce's prompt, held until the nonce's cost is sealed. See docs/accounting.md, "Money and tokens".
type PersistedInputCount struct {
	Nonce  uint64                    `json:"nonce"`
	Tokens uint64                    `json:"tokens"`
	Source filters.InputTokensSource `json:"source"`
}

func (e *escrowLedger) recordInputCount(attempt Attempt) {
	if !attempt.InputTokensSource.Tokenized() || e.isGhost(attempt.Nonce) {
		return
	}
	e.inputCounts[attempt.Nonce] = inputCount{tokens: attempt.InputTokens, source: attempt.InputTokensSource}
}

func (e *escrowLedger) dropSealedInputCounts() {
	for nonce := range e.inputCounts {
		if _, live := e.costs[nonce]; !live {
			delete(e.inputCounts, nonce)
		}
	}
}

func (e *escrowLedger) estimatedInput(nonce uint64, cost nonceCost) (tokens uint64, tokenized bool) {
	if counted, known := e.inputCounts[nonce]; known {
		return counted.tokens, true
	}
	return filters.EstimatedPromptTokens(int(cost.inputLength)), false
}

func (e *escrowLedger) persistedInputCounts() []PersistedInputCount {
	counts := make([]PersistedInputCount, 0, len(e.inputCounts))
	for nonce, counted := range e.inputCounts {
		counts = append(counts, PersistedInputCount{Nonce: nonce, Tokens: counted.tokens, Source: counted.source})
	}
	slices.SortFunc(counts, func(left, right PersistedInputCount) int { return cmp.Compare(left.Nonce, right.Nonce) })
	return counts
}

func (e *escrowLedger) restoreInputCounts(counts []PersistedInputCount) {
	for _, counted := range counts {
		e.inputCounts[counted.Nonce] = inputCount{tokens: counted.Tokens, source: counted.Source}
	}
}

package accounting

import (
	"devshard/cmd/gateway/filters"
	"devshard/types"
)

// nonceCost is one nonce's money as the escrow recorded it. See docs/accounting.md, "Money and tokens".
type nonceCost struct {
	reserved    uint64
	actual      uint64
	inputLength uint64
	maxTokens   uint64
	input       uint64
	output      uint64
	status      types.InferenceStatus
}

// SlotMoney is one slot's share of the escrow's money. See docs/accounting.md, "Money and tokens".
type SlotMoney struct {
	Reserved       uint64 `json:"reserved_cost,omitempty"`
	Actual         uint64 `json:"actual_cost,omitempty"`
	Refunded       uint64 `json:"refunded_cost,omitempty"`
	EstimatedInput uint64 `json:"estimated_input_tokens,omitempty"`
	EstimatedError uint64 `json:"estimated_error_tokens,omitempty"`
	MaxTokens      uint64 `json:"max_tokens,omitempty"`
	Input          uint64 `json:"input_tokens,omitempty"`
	CountedNonces  uint64 `json:"counted_nonces,omitempty"`
}

func (m *SlotMoney) add(other SlotMoney) {
	m.Reserved += other.Reserved
	m.Actual += other.Actual
	m.Refunded += other.Refunded
	m.EstimatedInput += other.EstimatedInput
	m.EstimatedError += other.EstimatedError
	m.MaxTokens += other.MaxTokens
	m.Input += other.Input
	m.CountedNonces += other.CountedNonces
}

func (c nonceCost) refunded() uint64 {
	switch c.status {
	case types.StatusTimedOut, types.StatusInvalidated:
		return c.reserved
	case types.StatusPending, types.StatusStarted:
		return 0
	}
	if c.actual > c.reserved {
		return 0
	}
	return c.reserved - c.actual
}

// foldMoney gives every slot its share of the escrow: what the chain says now, over the fold last saved for it.
func (e *escrowLedger) foldMoney() []SlotMoney {
	money := make([]SlotMoney, len(e.metadata.Slots))
	for nonce, cost := range e.costs {
		e.foldCost(&money[e.slotOf(nonce)], nonce, cost)
	}
	for slotID, saved := range e.folded {
		if int(slotID) < len(money) {
			money[slotID].add(saved)
		}
	}
	return money
}

// addProduced counts an attempt's tokens once, on the slot that produced them. See docs/accounting.md, "Money and tokens".
func (e *escrowLedger) addProduced(nonce uint64, record *nonceRecord, tokens int64) {
	if record.outputAdded || tokens <= 0 {
		return
	}
	record.outputAdded = true
	e.produced[e.slotOf(nonce)] += uint64(tokens)
}

func (e *escrowLedger) foldCost(money *SlotMoney, nonce uint64, cost nonceCost) {
	money.Reserved += cost.reserved
	money.Actual += cost.actual
	money.Refunded += cost.refunded()
	if e.isGhost(nonce) {
		return
	}
	if cost.status != types.StatusFinished && cost.status != types.StatusValidated {
		money.EstimatedError += filters.EstimatedPromptTokens(int(cost.inputLength))
		return
	}
	money.CountedNonces++
	money.Input += cost.input
	money.EstimatedInput += filters.EstimatedPromptTokens(int(cost.inputLength))
	money.MaxTokens += cost.maxTokens
}

package app

import (
	"context"
	"testing"

	inferencetypes "github.com/productscience/inference/x/inference/types"
	"google.golang.org/grpc"

	"devshard/cmd/gateway/chain"
)

type countingEpochGroupData struct{ calls int }

func (query *countingEpochGroupData) EpochGroupData(context.Context, *inferencetypes.QueryGetEpochGroupDataRequest, ...grpc.CallOption) (*inferencetypes.QueryGetEpochGroupDataResponse, error) {
	query.calls++
	return &inferencetypes.QueryGetEpochGroupDataResponse{}, nil
}

// Test flow:
//  1. Build the prompt counter without a chain query, and with one over a snapshot that has not seen an epoch yet.
//  2. Ask the second for a body's prompt tokens.
//  3. Assert the first is nil, so no prompt is checked, and the second answers unknown without reading a pin.
func TestThePromptCounterNeedsAChainAndAnEpoch(t *testing.T) {
	if counter := promptTokens(nil, fixedPhase{chain.PhaseSnapshot{EpochIndex: 7}}); counter != nil {
		t.Fatalf("prompt counter without a chain = %v, want nil", counter)
	}
	query := &countingEpochGroupData{}
	counter := promptTokens(query, fixedPhase{chain.PhaseSnapshot{}})

	_, known := counter.MinimumPromptTokens("Qwen/Test", []byte(`{"model":"Qwen/Test","messages":[{"role":"user","content":"hi"}]}`))

	if known || query.calls != 0 {
		t.Fatalf("known = %v after %d pin reads before the first epoch, want false after none", known, query.calls)
	}
}

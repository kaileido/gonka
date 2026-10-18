package registry

import (
	"testing"

	"devshard/cmd/gateway/filters"
	"devshard/cmd/gateway/scheduler"
	"devshard/state"
	"devshard/user"
)

// Test flow:
//  1. For each of four request bodies (the table varies whether max_tokens, max_completion_tokens, or neither is set), normalize the request through `filters.NormalizeRequest`.
//  2. Assert normalization succeeds and the resulting MaxTokens is at or above `state.MinReservationTokens`, the chain's own floor.
//  3. Advance a live stream, committing a nonce intent built from the normalized body.
//  4. Assert the commit succeeds and a nonce was prepared.
func TestWhatTheFiltersProduceTheChainAccepts(t *testing.T) {
	for _, body := range []string{
		`{"messages":[{"role":"user","content":"x"}],"max_tokens":1}`,
		`{"messages":[{"role":"user","content":"x"}],"max_tokens":8}`,
		`{"messages":[{"role":"user","content":"x"}],"max_completion_tokens":8}`,
		`{"messages":[{"role":"user","content":"x"}]}`,
	} {
		t.Run(body, func(t *testing.T) {
			normalized, err := filters.NormalizeRequest([]byte(body), filters.Options{
				RoutedModel:      "qwen",
				DefaultMaxTokens: 3072,
				MaxTokensCap:     4096,
			})
			if err != nil {
				t.Fatalf("NormalizeRequest() = %v, want nil", err)
			}
			if normalized.MaxTokens < state.MinReservationTokens {
				t.Fatalf("MaxTokens = %d, under the chain's floor %d before the chain ever sees it", normalized.MaxTokens, state.MinReservationTokens)
			}

			stream, _, _ := newLiveStream(t, 1, 1, 1)
			prepared, err := stream.Advance(func(scheduler.HostBinding) scheduler.NonceIntent {
				return scheduler.NonceIntent{Commit: true, Params: user.InferenceParams{
					Model:       "qwen",
					Prompt:      normalized.Body,
					InputLength: uint64(len(normalized.Body)),
					MaxTokens:   normalized.MaxTokens,
				}}
			})
			if err != nil {
				t.Fatalf("committing what the filters produced = %v, want nil", err)
			}
			if prepared == nil {
				t.Fatal("no nonce was committed")
			}
		})
	}
}

// Test flow:
//  1. Build a live stream.
//  2. Advance it, committing a nonce intent built from the stream's ghost params.
//  3. Assert the commit succeeds and a nonce was prepared.
func TestAGhostBurnStillCommits(t *testing.T) {
	stream, _, _ := newLiveStream(t, 1, 1, 1)

	prepared, err := stream.Advance(func(scheduler.HostBinding) scheduler.NonceIntent {
		return scheduler.NonceIntent{Commit: true, Params: stream.ghostParams()}
	})
	if err != nil {
		t.Fatalf("burning a nonce = %v, want nil", err)
	}
	if prepared == nil {
		t.Fatal("no nonce was burned")
	}
}

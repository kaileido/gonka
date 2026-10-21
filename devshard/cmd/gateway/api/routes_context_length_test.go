package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"devshard/cmd/gateway/chain"
	"devshard/cmd/gateway/config"
)

type fakePromptTokens struct {
	tokens uint64
	known  bool
	model  string
	body   string
}

func (f *fakePromptTokens) Estimate(model string, body []byte) (uint64, bool) {
	f.model, f.body = model, string(body)
	return f.tokens, f.known
}

func pinContextLength(length int64) func(*config.Config) {
	return func(c *config.Config) {
		c.Limits.ModelLimits = map[string]config.ModelLimits{"qwen": {MaxModelLen: &length}}
	}
}

// Test flow:
//  1. Pin qwen's context length at 20 000 tokens and wire a prompt estimate of 3 616, exactly the context length less the 16 384 reserve.
//  2. Send a chat request.
//  3. Assert it is refused 400 with the OpenAI type and code context_length_exceeded, and a message naming the context length and the estimate.
//  4. Assert the estimate was asked for the normalized body of the requested model, and that neither the limiter nor the race was reached, so no nonce exists.
func TestAPromptPastTheContextLessTheReserveIsRefusedBeforeTheRace(t *testing.T) {
	live := newHarness(t, pinContextLength(20_000))
	estimate := &fakePromptTokens{tokens: 20_000 - promptContextReserve, known: true}
	live.server.promptTokens = estimate

	recorder := live.request(t, http.MethodPost, "/v1/chat/completions", chatBody, nil)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d (%s), want 400", recorder.Code, recorder.Body.String())
	}
	var envelope errorEnvelope
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode %s: %v", recorder.Body.String(), err)
	}
	if envelope.Error.Type != errorTypeInvalidRequest || envelope.Error.Code != errorCodeContextLengthExceeded {
		t.Fatalf("error = %+v, want type %q and code %q", envelope.Error, errorTypeInvalidRequest, errorCodeContextLengthExceeded)
	}
	if !strings.Contains(envelope.Error.Message, "20000 tokens") || !strings.Contains(envelope.Error.Message, "about 3616 tokens") {
		t.Fatalf("message = %q, want the context length and the estimate", envelope.Error.Message)
	}
	if estimate.model != "qwen" || !strings.Contains(estimate.body, `"stream":true`) {
		t.Fatalf("estimated model %q body %s, want qwen's normalized body", estimate.model, estimate.body)
	}
	if live.limiter.acquires.Load() != 0 || live.inference.raced.Load() != nil {
		t.Fatalf("limiter acquires = %d, raced = %v: a refused prompt must not reach the limiter or the race", live.limiter.acquires.Load(), live.inference.raced.Load() != nil)
	}
}

// Test flow:
//  1. Define cases that must be served: an estimate one token under the limit, an unknown estimate, a model with no context length, a governance length the reserve swallows whole, and no estimator wired.
//  2. For each, send a chat request through a fresh harness.
//  3. Assert it is served and reaches the race.
func TestAPromptTheCheckCannotOrNeedNotRefuseIsServed(t *testing.T) {
	testCases := []struct {
		name     string
		tune     func(*config.Config)
		governed uint64
		estimate *fakePromptTokens
	}{
		{name: "under the limit", tune: pinContextLength(20_000), estimate: &fakePromptTokens{tokens: 20_000 - promptContextReserve - 1, known: true}},
		{name: "unknown estimate", tune: pinContextLength(20_000), estimate: &fakePromptTokens{tokens: 1 << 30}},
		{name: "no context length", tune: func(*config.Config) {}, estimate: &fakePromptTokens{tokens: 1 << 30, known: true}},
		{name: "governed length inside the reserve", tune: func(*config.Config) {}, governed: promptContextReserve, estimate: &fakePromptTokens{tokens: 1 << 30, known: true}},
		{name: "no estimator", tune: pinContextLength(20_000)},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			live := newHarness(t, testCase.tune)
			live.snapshots.snapshot = chain.PhaseSnapshot{Models: map[string]chain.ModelParams{"qwen": {MaxModelLen: testCase.governed}}}
			if testCase.estimate != nil {
				live.server.promptTokens = testCase.estimate
			}

			recorder := live.request(t, http.MethodPost, "/v1/chat/completions", chatBody, nil)

			if recorder.Code != http.StatusOK || live.inference.raced.Load() == nil {
				t.Fatalf("status = %d (%s), raced = %v, want served", recorder.Code, recorder.Body.String(), live.inference.raced.Load() != nil)
			}
		})
	}
}

// Test flow:
//  1. Leave qwen without an operator pin and let governance report a --max-model-len of 40 000.
//  2. Wire an estimate past that length less the reserve and send a chat request.
//  3. Assert it is refused 400, so the governed length is the one the check reads.
func TestTheCheckReadsTheGovernedContextLength(t *testing.T) {
	live := newHarness(t)
	live.snapshots.snapshot = chain.PhaseSnapshot{Models: map[string]chain.ModelParams{"qwen": {MaxModelLen: 40_000}}}
	live.server.promptTokens = &fakePromptTokens{tokens: 40_000 - promptContextReserve, known: true}

	recorder := live.request(t, http.MethodPost, "/v1/chat/completions", chatBody, nil)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d (%s), want 400", recorder.Code, recorder.Body.String())
	}
}

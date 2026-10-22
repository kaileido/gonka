package api

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/text/unicode/norm"

	"devshard/cmd/gateway/chain"
	"devshard/cmd/gateway/config"
	"devshard/cmd/gateway/filters"
	"devshard/cmd/gateway/tokenizer"
)

type fakePromptTokens struct {
	tokens     uint64
	slack      uint64
	known      bool
	model      string
	body       string
	tokenizing int
}

func (f *fakePromptTokens) Prompt(model string, body []byte) (PromptCount, bool) {
	f.model, f.body = model, string(body)
	return f, f.known
}

func (f *fakePromptTokens) MaximumTokens() uint64 { return f.tokens + f.slack }

func (f *fakePromptTokens) MinimumTokens() (uint64, bool) {
	f.tokenizing++
	return f.tokens, true
}

type tokenizedOnly struct{ PromptCount }

func (tokenizedOnly) MaximumTokens() uint64 { return math.MaxUint32 }

func loadTestTokenizer(t *testing.T, name string) *tokenizer.Counter {
	t.Helper()
	file, err := os.Open(filepath.Join("..", "tokenizer", "testdata", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	counter, err := tokenizer.Load(file)
	if err != nil {
		t.Fatal(err)
	}
	return counter
}

func randomPromptText(size int, rng *rand.Rand) string {
	fragments := []string{"the ", "Шлюз ", "网关", "😀", "é", "क़", "שּׁ", "\U0001d160", "<|im_start|>", "\n\n", `{"id":1}`}
	var builder strings.Builder
	for builder.Len() < size {
		builder.WriteString(fragments[rng.IntN(len(fragments))])
	}
	return builder.String()
}

func pinContextLength(length int64) func(*config.Config) {
	return func(c *config.Config) {
		c.Limits.ModelLimits = map[string]config.ModelLimits{"qwen": {MaxModelLen: &length}}
	}
}

func chatBodyWith(field string, value uint64) string {
	return fmt.Sprintf(`{"model":"qwen","%s":%d,"messages":[{"role":"user","content":"hi"}]}`, field, value)
}

func decodeErrorDetail(t *testing.T, body []byte) errorDetail {
	t.Helper()
	var envelope errorEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return envelope.Error
}

// Test flow:
//  1. Pin qwen's context length at 20 000 tokens and define requests with the default max_tokens, an explicit max_tokens and an explicit max_completion_tokens.
//  2. For each, wire a prompt count that leaves exactly max_tokens and the template allowance, and one token more.
//  3. Assert the exact fit is served and the one token more is refused 400 with OpenAI's type and code context_length_exceeded.
func TestTheCheckRefusesExactlyWhatTheContextCannotHoldWithMaxTokens(t *testing.T) {
	const contextLength = 20_000
	testCases := []struct {
		name      string
		body      string
		maxTokens uint64
	}{
		{name: "default max_tokens", body: chatBody, maxTokens: filters.DefaultRequestMaxTokens},
		{name: "explicit max_tokens", body: chatBodyWith("max_tokens", 100), maxTokens: 100},
		{name: "explicit max_completion_tokens", body: chatBodyWith("max_completion_tokens", 100), maxTokens: 100},
	}
	for _, testCase := range testCases {
		fits := uint64(contextLength) - testCase.maxTokens - chatTemplateMinimumTokens
		for _, step := range []struct {
			name   string
			tokens uint64
			status int
		}{{"exact fit", fits, http.StatusOK}, {"one token over", fits + 1, http.StatusBadRequest}} {
			t.Run(testCase.name+"/"+step.name, func(t *testing.T) {
				live := newHarness(t, pinContextLength(contextLength))
				live.server.promptTokens = &fakePromptTokens{tokens: step.tokens, known: true}

				recorder := live.request(t, http.MethodPost, "/v1/chat/completions", testCase.body, nil)

				if recorder.Code != step.status {
					t.Fatalf("status = %d (%s), want %d", recorder.Code, recorder.Body.String(), step.status)
				}
				if step.status == http.StatusBadRequest {
					detail := decodeErrorDetail(t, recorder.Body.Bytes())
					if detail.Type != errorTypeInvalidRequest || detail.Code != errorCodeContextLengthExceeded {
						t.Fatalf("error = %+v, want type %q and code %q", detail, errorTypeInvalidRequest, errorCodeContextLengthExceeded)
					}
				}
			})
		}
	}
}

// Test flow:
//  1. Raise the max_tokens cap, pin qwen's context length at 400 000 tokens and wire a prompt count of 380 000.
//  2. Send a chat request with max_tokens 30 000.
//  3. Assert it is refused 400 with a message naming the context length, max_tokens, the prompt, the total and both ways out.
//  4. Assert the count was asked for the normalized body of the requested model, and that neither the limiter nor the race was reached, so no nonce exists.
func TestAPromptAndMaxTokensPastTheContextAreRefusedBeforeTheRace(t *testing.T) {
	live := newHarness(t, pinContextLength(400_000), func(c *config.Config) { c.Limits.MaxTokensCap = 64_000 })
	count := &fakePromptTokens{tokens: 380_000, known: true}
	live.server.promptTokens = count

	recorder := live.request(t, http.MethodPost, "/v1/chat/completions", chatBodyWith("max_tokens", 30_000), nil)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d (%s), want 400", recorder.Code, recorder.Body.String())
	}
	message := decodeErrorDetail(t, recorder.Body.Bytes()).Message
	for _, want := range []string{"is 400000 tokens", "up to 30000 output tokens", "at least 380004 tokens", "at least 410004 in all", "by at least 10004 tokens", "lower max_tokens to at most 19996"} {
		if !strings.Contains(message, want) {
			t.Fatalf("message = %q, want it to name %q", message, want)
		}
	}
	if count.model != "qwen" || !strings.Contains(count.body, `"stream":true`) {
		t.Fatalf("counted model %q body %s, want qwen's normalized body", count.model, count.body)
	}
	if live.limiter.acquires.Load() != 0 || live.inference.raced.Load() != nil {
		t.Fatalf("limiter acquires = %d, raced = %v: a refused prompt must not reach the limiter or the race", live.limiter.acquires.Load(), live.inference.raced.Load() != nil)
	}
}

// Test flow:
//  1. Pin qwen's context length at 20 000 tokens and wire a prompt count of 25 000, past the context on its own.
//  2. Send a chat request.
//  3. Assert the refusal asks for shorter messages and offers no max_tokens that would fit.
func TestAPromptPastTheContextOnItsOwnIsToldToShortenTheMessagesOnly(t *testing.T) {
	live := newHarness(t, pinContextLength(20_000))
	live.server.promptTokens = &fakePromptTokens{tokens: 25_000, known: true}

	recorder := live.request(t, http.MethodPost, "/v1/chat/completions", chatBody, nil)

	message := decodeErrorDetail(t, recorder.Body.Bytes()).Message
	if recorder.Code != http.StatusBadRequest || !strings.Contains(message, "Shorten the messages by at least 9100 tokens.") || strings.Contains(message, "max_tokens to") {
		t.Fatalf("status = %d, message = %q, want 400 asking for 9100 fewer message tokens only", recorder.Code, message)
	}
}

// Test flow:
//  1. Define cases the check cannot judge: a model without a tokenizer, a model with no context length, and no counter wired.
//  2. For each, send a chat request whose count, where there is one, is past any context, through a fresh harness.
//  3. Assert it is served and reaches the race.
func TestARequestTheCheckCannotJudgeIsServed(t *testing.T) {
	testCases := []struct {
		name  string
		tune  func(*config.Config)
		count *fakePromptTokens
	}{
		{name: "model without a tokenizer", tune: pinContextLength(20_000), count: &fakePromptTokens{tokens: 1 << 30}},
		{name: "model without a context length", tune: func(*config.Config) {}, count: &fakePromptTokens{tokens: 1 << 30, known: true}},
		{name: "no counter", tune: pinContextLength(20_000)},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			live := newHarness(t, testCase.tune)
			if testCase.count != nil {
				live.server.promptTokens = testCase.count
			}

			recorder := live.request(t, http.MethodPost, "/v1/chat/completions", chatBody, nil)

			if recorder.Code != http.StatusOK || live.inference.raced.Load() == nil {
				t.Fatalf("status = %d (%s), raced = %v, want served", recorder.Code, recorder.Body.String(), live.inference.raced.Load() != nil)
			}
		})
	}
}

// Test flow:
//  1. Pin qwen's context length at 20 000 tokens and let governance report a --max-model-len of 40 000.
//  2. Send one request whose prompt fits 40 000 but not 20 000, and one that fits neither.
//  3. Assert the first is served and the second refused, so the governed length is the one the check reads.
func TestTheCheckReadsTheGovernedContextLengthBeforeThePin(t *testing.T) {
	for _, step := range []struct {
		tokens uint64
		status int
	}{{30_000, http.StatusOK}, {40_000, http.StatusBadRequest}} {
		live := newHarness(t, pinContextLength(20_000))
		live.snapshots.snapshot = chain.PhaseSnapshot{Models: map[string]chain.ModelParams{"qwen": {MaxModelLen: 40_000}}}
		live.server.promptTokens = &fakePromptTokens{tokens: step.tokens, known: true}

		recorder := live.request(t, http.MethodPost, "/v1/chat/completions", chatBody, nil)

		if recorder.Code != step.status {
			t.Fatalf("prompt %d: status = %d (%s), want %d", step.tokens, recorder.Code, recorder.Body.String(), step.status)
		}
	}
}

// Test flow:
//  1. Pin qwen's context length at 20 000 tokens and define prompts whose byte bound fits it, whose bound is past it while their count fits, and whose count is past it too.
//  2. Send a chat request with each.
//  3. Assert the first is served without tokenizing, the second is served after tokenizing once, and the third is refused after tokenizing once.
func TestThePromptIsTokenizedOnlyWhenItsByteBoundIsPastTheContext(t *testing.T) {
	fits := uint64(20_000) - filters.DefaultRequestMaxTokens - chatTemplateMinimumTokens
	for _, step := range []struct {
		name       string
		count      *fakePromptTokens
		status     int
		tokenizing int
	}{
		{name: "bound fits", count: &fakePromptTokens{tokens: fits, known: true}, status: http.StatusOK},
		{name: "bound over, count fits", count: &fakePromptTokens{tokens: fits, slack: 1, known: true}, status: http.StatusOK, tokenizing: 1},
		{name: "count over", count: &fakePromptTokens{tokens: fits + 1, known: true}, status: http.StatusBadRequest, tokenizing: 1},
	} {
		t.Run(step.name, func(t *testing.T) {
			live := newHarness(t, pinContextLength(20_000))
			live.server.promptTokens = step.count

			recorder := live.request(t, http.MethodPost, "/v1/chat/completions", chatBody, nil)

			if recorder.Code != step.status || step.count.tokenizing != step.tokenizing {
				t.Fatalf("status = %d, tokenized %d times, want %d after %d", recorder.Code, step.count.tokenizing, step.status, step.tokenizing)
			}
		})
	}
}

// Test flow:
//  1. Load the NFC and a plain testdata tokenizer, and build chat bodies of random ASCII, Cyrillic, CJK, emoji, special-token spellings, decomposed and NFC-expanding text, as written and decomposed, from empty to past 64 KiB.
//  2. For each, take context lengths around the prompt's count and its byte bound, and judge the request with the byte bound and with the tokenizer alone.
//  3. Assert both judgements are the same for every request, refusals included.
func TestTheByteBoundNeverChangesTheJudgement(t *testing.T) {
	live := newHarness(t)
	rng := rand.New(rand.NewPCG(3, 4))
	for _, name := range []string{"removed-nfc", "sequence"} {
		counter := counterPromptTokens{counter: loadTestTokenizer(t, name)}
		for _, size := range []int{0, 1, 200, 4 << 10, 70 << 10} {
			for _, text := range []string{randomPromptText(size, rng), norm.NFD.String(randomPromptText(size, rng))} {
				content, err := json.Marshal(text)
				if err != nil {
					t.Fatal(err)
				}
				body := []byte(`{"model":"qwen","messages":[{"role":"user","content":` + string(content) + `}]}`)
				prompt, _ := counter.Prompt("qwen", body)
				count, _ := prompt.MinimumTokens()
				for _, maxTokens := range []uint64{1, filters.DefaultRequestMaxTokens} {
					for _, edge := range []uint64{count, prompt.MaximumTokens()} {
						for _, length := range []uint64{edge + chatTemplateMinimumTokens + maxTokens - 1, edge + chatTemplateMinimumTokens + maxTokens, edge + chatTemplateMinimumTokens + maxTokens + 1} {
							pinned := int64(length)
							limits := config.Limits{ModelLimits: map[string]config.ModelLimits{"qwen": {MaxModelLen: &pinned}}}
							normalized := filters.Result{Model: "qwen", Body: body, MaxTokens: maxTokens}

							live.server.promptTokens = counter
							withBound := live.server.fitsContext(limits, normalized)
							live.server.promptTokens = tokenizedOnlyPrompts{counter}
							tokenized := live.server.fitsContext(limits, normalized)

							if !reflect.DeepEqual(withBound, tokenized) {
								t.Fatalf("%s, %d bytes, max_tokens %d, context %d: with the bound %v, tokenized alone %v", name, len(text), maxTokens, length, withBound, tokenized)
							}
						}
					}
				}
			}
		}
	}
}

type tokenizedOnlyPrompts struct{ PromptTokens }

func (t tokenizedOnlyPrompts) Prompt(model string, body []byte) (PromptCount, bool) {
	prompt, known := t.PromptTokens.Prompt(model, body)
	return tokenizedOnly{prompt}, known
}

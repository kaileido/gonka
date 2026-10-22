package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"devshard/cmd/gateway/config"
	"devshard/cmd/gateway/filters"
	"devshard/cmd/gateway/tokenizer"
)

type counterPromptTokens struct{ counter *tokenizer.Counter }

type counterPrompt struct {
	counter *tokenizer.Counter
	parts   []string
}

func (c counterPromptTokens) Prompt(_ string, body []byte) (PromptCount, bool) {
	parts, err := filters.PromptText(body)
	if err != nil {
		return nil, false
	}
	return counterPrompt{counter: c.counter, parts: parts}, true
}

func (c counterPrompt) MaximumTokens() uint64 { return c.counter.MaximumTokens(c.parts) }

func (c counterPrompt) MinimumTokens() (uint64, bool) {
	tokens, err := c.counter.MinimumTokens(c.parts)
	return tokens, err == nil
}

// Test flow:
//  1. Load every tokenizer.json in GATEWAY_TOKENIZER_DIR, and skip when it names none.
//  2. Build the benchmark server with qwen pinned to a context the prompts' byte bound fits, and to one only their count fits, half their bytes, so the check passes with and without tokenizing.
//  3. Serve a buffered chat request of a 200 B and a 1 MiB prompt, without the check and with it for each tokenizer and context, and report the handler's time and allocations.
func BenchmarkChatContextCheck(b *testing.B) {
	dir := os.Getenv("GATEWAY_TOKENIZER_DIR")
	if dir == "" {
		b.Skip("GATEWAY_TOKENIZER_DIR names no directory of tokenizer.json files")
	}
	paths, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	estimators := map[string]PromptTokens{"off": nil}
	for _, path := range paths {
		file, err := os.Open(path)
		if err != nil {
			b.Fatal(err)
		}
		counter, err := tokenizer.Load(file)
		_ = file.Close()
		if err != nil {
			b.Fatal(err)
		}
		estimators[strings.TrimSuffix(filepath.Base(path), ".json")] = counterPromptTokens{counter: counter}
	}
	for _, size := range []struct {
		name  string
		bytes int
	}{{"200B", 200}, {"1MiB", 1 << 20}} {
		body := benchChatBody(false, size.bytes)
		contexts := map[string]int64{
			"bound":     config.MaxContextTokens,
			"tokenized": int64(size.bytes/2) + chatTemplateMinimumTokens + int64(filters.DefaultRequestMaxTokens),
		}
		for contextName, pinned := range contexts {
			for name, estimator := range estimators {
				b.Run(size.name+"/"+contextName+"/"+name, func(b *testing.B) {
					server := benchServer(b, benchChunks(48, 24))
					next := *server.config.Load()
					next.Limits.ModelLimits = map[string]config.ModelLimits{"qwen": {MaxModelLen: &pinned}}
					server.config.Swap(&next)
					server.promptTokens = estimator
					serveRepeatedly(b, server.Handler(), benchRequest("/v1/chat/completions", body), body, http.StatusOK)
				})
			}
		}
	}
}

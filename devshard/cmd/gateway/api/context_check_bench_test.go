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

func (c counterPromptTokens) MinimumPromptTokens(_ string, body []byte) (uint64, bool) {
	prompt, err := filters.PromptText(body)
	if err != nil {
		return 0, false
	}
	tokens, err := c.counter.MinimumTokens(prompt)
	return tokens, err == nil
}

// Test flow:
//  1. Load every tokenizer.json in GATEWAY_TOKENIZER_DIR, and skip when it names none.
//  2. Build the benchmark server with qwen pinned to a context the prompts fit, so the check runs and passes.
//  3. Serve a buffered chat request of a 200 B and a 1 MiB prompt, without the check and with it for each tokenizer, and report the handler's time and allocations.
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
	pinned := int64(config.MaxContextTokens)
	for _, size := range []struct {
		name  string
		bytes int
	}{{"200B", 200}, {"1MiB", 1 << 20}} {
		body := benchChatBody(false, size.bytes)
		for name, estimator := range estimators {
			b.Run(size.name+"/"+name, func(b *testing.B) {
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

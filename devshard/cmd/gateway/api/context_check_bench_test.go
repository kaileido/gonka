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

var benchPromptSizes = []struct {
	name  string
	bytes int
}{{"200B", 200}, {"1MiB", 1 << 20}}

func benchCounters(b *testing.B) map[string]*tokenizer.Counter {
	b.Helper()
	dir := os.Getenv("GATEWAY_TOKENIZER_DIR")
	if dir == "" {
		b.Skip("GATEWAY_TOKENIZER_DIR names no directory of tokenizer.json files")
	}
	paths, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	counters := make(map[string]*tokenizer.Counter, len(paths))
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
		counters[strings.TrimSuffix(filepath.Base(path), ".json")] = counter
	}
	return counters
}

// Test flow:
//  1. Load every tokenizer.json in GATEWAY_TOKENIZER_DIR, and skip when it names none.
//  2. Build the benchmark server with qwen pinned to a context the prompts' byte bound fits, and to one only their count fits, half their bytes, so the check passes with and without tokenizing.
//  3. Serve a buffered chat request of a 200 B and a 1 MiB prompt, without the check and with it for each tokenizer and context, and report the handler's time and allocations.
func BenchmarkChatContextCheck(b *testing.B) {
	estimators := map[string]PromptTokens{"off": nil}
	for name, counter := range benchCounters(b) {
		estimators[name] = counterPromptTokens{counter: counter}
	}
	for _, size := range benchPromptSizes {
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

// Test flow:
//  1. Load every tokenizer.json in GATEWAY_TOKENIZER_DIR, and skip when it names none.
//  2. For a chat body of a 200 B and a 1 MiB prompt and each tokenizer, read the prompt and take its byte bound, and read it and tokenize it.
//  3. Report the time and allocations of each, the check's own cost without the handler around it.
func BenchmarkContextCheckCost(b *testing.B) {
	for name, counter := range benchCounters(b) {
		prompts := counterPromptTokens{counter: counter}
		for _, size := range benchPromptSizes {
			body := []byte(benchChatBody(false, size.bytes))
			b.Run(size.name+"/"+name+"/bound", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					prompt, _ := prompts.Prompt("qwen", body)
					_ = prompt.MaximumTokens()
				}
			})
			b.Run(size.name+"/"+name+"/tokenized", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					prompt, _ := prompts.Prompt("qwen", body)
					if _, known := prompt.MinimumTokens(); !known {
						b.Fatal("the prompt could not be tokenized")
					}
				}
			})
		}
	}
}

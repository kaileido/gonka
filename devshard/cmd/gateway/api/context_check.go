package api

import (
	"fmt"

	"devshard/cmd/gateway/config"
	"devshard/cmd/gateway/filters"
)

// See docs/request.md, "Step 6 in detail".
const (
	chatTemplateMinimumTokens = 4
)

// ContextLengthError is a request whose prompt and max_tokens a host's context cannot hold together, which the host would refuse.
type ContextLengthError struct {
	ContextLength uint64
	PromptTokens  uint64
	MaxTokens     uint64
}

func (e *ContextLengthError) Error() string {
	total := e.PromptTokens + e.MaxTokens
	message := fmt.Sprintf("This model's maximum context length is %d tokens. However, the request asks for up to %d output tokens (max_tokens) and its messages take at least %d tokens with the chat template, at least %d in all. Shorten the messages by at least %d tokens",
		e.ContextLength, e.MaxTokens, e.PromptTokens, total, total-e.ContextLength)
	if e.PromptTokens < e.ContextLength {
		message += fmt.Sprintf(", or lower max_tokens to at most %d", e.ContextLength-e.PromptTokens)
	}
	return message + "."
}

func (s *Server) fitsContext(configured config.Limits, normalized filters.Result) error {
	if s.promptTokens == nil {
		return nil
	}
	contextLength := configured.ContextLength(normalized.Model, s.snapshots.Snapshot().Models[normalized.Model].MaxModelLen)
	if contextLength == config.NoContextLength {
		return nil
	}
	prompt, known := s.promptTokens.Prompt(normalized.Model, normalized.Body)
	if !known || prompt.MaximumTokens()+chatTemplateMinimumTokens+normalized.MaxTokens <= contextLength {
		return nil
	}
	messageTokens, known := prompt.MinimumTokens()
	if !known {
		return nil
	}
	promptTokens := messageTokens + chatTemplateMinimumTokens
	if promptTokens+normalized.MaxTokens <= contextLength {
		return nil
	}
	return &ContextLengthError{ContextLength: contextLength, PromptTokens: promptTokens, MaxTokens: normalized.MaxTokens}
}

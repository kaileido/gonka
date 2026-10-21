package filters

import (
	"slices"
	"testing"
)

// Test flow:
//  1. Build a normalized body with a system and a user message, an assistant tool call with its arguments, a tool reply, a function_call in the deprecated shape, a null content, and a tools array.
//  2. Read its prompt text.
//  3. Assert the parts are each message's text, each call's name and arguments, and the tools array as its JSON, in order, with JSON escapes decoded.
func TestPromptTextIsWhatTheTemplateRenders(t *testing.T) {
	body := `{"model":"m","stream":true,"messages":[` +
		`{"role":"system","content":"be brief"},` +
		`{"role":"user","content":"line one\nline \"two\""},` +
		`{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"lookup","arguments":"{\"q\":1}"}}]},` +
		`{"role":"tool","tool_call_id":"c1","content":"found"},` +
		`{"role":"assistant","content":"done","function_call":{"name":"deprecated","arguments":"{}"}}],` +
		`"tools":[{"type":"function","function":{"name":"lookup"}}]}`

	parts, err := PromptText([]byte(body))
	if err != nil {
		t.Fatalf("PromptText: %v", err)
	}

	want := []string{"be brief", "line one\nline \"two\"", "lookup", `{"q":1}`, "found", "done", "deprecated", "{}", `[{"type":"function","function":{"name":"lookup"}}]`}
	if !slices.Equal(parts, want) {
		t.Fatalf("parts = %q, want %q", parts, want)
	}
}

// Test flow:
//  1. Read the prompt text of a body that is not JSON.
//  2. Assert it fails, so the caller leaves the prompt unchecked rather than counting nothing.
func TestPromptTextRefusesABodyItCannotDecode(t *testing.T) {
	if _, err := PromptText([]byte("{")); err == nil {
		t.Fatal("PromptText({) succeeded, want an error")
	}
}

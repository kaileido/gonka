package filters

import (
	"slices"
	"testing"
)

// Test flow:
//  1. Build a normalized body with a system and a user message, an assistant tool call with its arguments, a tool reply, a function_call in the deprecated shape, a null content, and a tools array whose description the body escapes as HTML.
//  2. Read its prompt text.
//  3. Assert the parts are each message's text, each call's name and arguments, and the tools array as compact JSON without HTML escapes and with its numbers as written, in order, with string escapes decoded.
func TestPromptTextIsWhatTheTemplateRenders(t *testing.T) {
	body := `{"model":"m","stream":true,"messages":[` +
		`{"role":"system","content":"be brief"},` +
		`{"role":"user","content":"line one\nline \"two\""},` +
		`{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"lookup","arguments":"{\"q\":1}"}}]},` +
		`{"role":"tool","tool_call_id":"c1","content":"found"},` +
		`{"role":"assistant","content":"done","function_call":{"name":"deprecated","arguments":"{}"}}],` +
		`"tools":[{"type":"function","function":{"name":"lookup","description":"a \u003cb\u003e \u0026 c","strict":1.50}}]}`

	parts, err := PromptText([]byte(body))
	if err != nil {
		t.Fatalf("PromptText: %v", err)
	}

	want := []string{"be brief", "line one\nline \"two\"", "lookup", `{"q":1}`, "found", "done", "deprecated", "{}", `[{"function":{"description":"a <b> & c","name":"lookup","strict":1.50},"type":"function"}]`}
	if !slices.Equal(parts, want) {
		t.Fatalf("parts = %q, want %q", parts, want)
	}
}

// Test flow:
//  1. Normalize a request whose user message is two text parts, as a client sends them.
//  2. Read the normalized body's prompt text.
//  3. Assert the message is one part, the texts joined as the host receives them, not the parts' JSON.
func TestPromptTextReadsTextPartsAsTheirText(t *testing.T) {
	result, err := NormalizeRequest([]byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"first \"one\""},{"type":"text","text":"second"}]}]}`), Options{})
	if err != nil {
		t.Fatal(err)
	}

	parts, err := PromptText(result.Body)
	if err != nil {
		t.Fatalf("PromptText: %v", err)
	}

	if want := []string{"first \"one\"\nsecond"}; !slices.Equal(parts, want) {
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

package filters

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func foldEvents(t *testing.T, events [][]byte, intent LogprobIntent, withTemplate bool) []byte {
	t.Helper()
	folder := NewBodyFolder(intent)
	for _, event := range events {
		if !withTemplate {
			folder.template = nil
		}
		if _, err := folder.Write(event); err != nil {
			t.Fatalf("Write(): %v", err)
		}
	}
	return folder.Body()
}

func topLevelKeyCount(payloads ...[]byte) int {
	keys := map[string]bool{}
	for _, payload := range payloads {
		decoded, _ := decodeStreamedEvent(bytes.TrimSpace(payload))
		for key := range decoded {
			keys[key] = true
		}
	}
	return len(keys)
}

func plainContentStream(chunks int) [][]byte {
	events := make([][]byte, 0, chunks+2)
	for index := range chunks {
		events = append(events, fmt.Appendf(nil, `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1700000000,`+
			`"model":"model-a","choices":[{"index":0,"delta":{"content":" token%d"},"logprobs":null,"finish_reason":null}]}`+"\n\n", index))
	}
	events = append(events, []byte(`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1700000000,`+
		`"model":"model-a","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":17,"completion_tokens":64,"total_tokens":81}}`+"\n\n"))
	return append(events, SSEDoneEvent)
}

// Test flow:
//  1. Fold a stream of plain content chunks, whose events differ only in their content text, with the template on and with it cleared before every event.
//  2. Assert both folds arrive at the same body, and that it carries every token in order.
func TestContentTemplateFoldsWhatTheFullMergeWould(t *testing.T) {
	events := plainContentStream(300)

	templated := foldEvents(t, events, LogprobIntent{}, true)
	merged := foldEvents(t, events, LogprobIntent{}, false)

	if !bytes.Equal(templated, merged) {
		t.Fatalf("templated fold = %s, want %s", templated, merged)
	}
	if !bytes.Contains(templated, []byte(`" token0 token1 token2`)) || !bytes.Contains(templated, []byte(`token298 token299"`)) {
		t.Fatalf("folded body lost tokens: %s", templated)
	}
}

// Test flow:
//  1. Fold a chunk naming as many choices as the fold holds, then two plain chunks for choice 0, with the template on and with it cleared before every event.
//  2. Assert both folds arrive at the same body: past the choice limit the full merge drops a chunk, so the template must not grow its text either.
func TestContentTemplateRespectsTheChoiceLimit(t *testing.T) {
	choices := make([]string, maxIndexedElements)
	for index := range choices {
		choices[index] = fmt.Sprintf(`{"index":%d,"delta":{"content":"c%d"}}`, index, index)
	}
	events := [][]byte{
		[]byte(`data: {"object":"chat.completion.chunk","choices":[` + strings.Join(choices, ",") + "]}\n\n"),
		[]byte(`data: {"object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"a"}}]}` + "\n\n"),
		[]byte(`data: {"object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"b"}}]}` + "\n\n"),
	}

	templated, merged := foldEvents(t, events, LogprobIntent{}, true), foldEvents(t, events, LogprobIntent{}, false)

	if !bytes.Equal(templated, merged) {
		t.Fatalf("templated fold differs from the full merge past the choice limit:\ntemplated %.200s\nmerged    %.200s", templated, merged)
	}
}

// Test flow:
//  1. Generate three payloads, skipping those past maxTopLevelFields keys between them, where the full merge keeps an arbitrary subset, and fold them as one stream: first, the second twice, third, then first and third again, once with the template and once with it cleared before every event.
//  2. Assert both folds arrive at the same body: the template only ever skips a decode whose merge would have appended the same text.
func FuzzContentTemplateAgreesWithTheFullMerge(f *testing.F) {
	plain := func(text string) []byte {
		return fmt.Appendf(nil, `{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"%s"},"logprobs":null,"finish_reason":null}]}`, text)
	}
	f.Add(plain("a"), plain(`b\u0041`), []byte(`{"choices":[{"index":0,"delta":{"content":"a"},"logprobs":{"content":[]}}]}`), false, false)
	f.Add([]byte(`{"choices":[{"index":0,"delta":{"content":"a","role":null}}]}`), []byte(`{"choices":[{"index":0,"delta":{"content":"b","role":null}}]}`), []byte(`{"choices":[{"index":0,"delta":{"content":"b\u0000","role":null}}]}`), true, false)
	f.Add([]byte(`{"choices":[{"index":0,"delta":{"content":"a","tool_calls":[]}}]}`), []byte(`{"choices":[{"index":0,"delta":{"content":"a"}}],"value":NaN}`), []byte(`{"choices":[{"index":0,"delta":{"content":"a\"},"x":{"y":"z"}}]}`), true, true)
	f.Add(plain("a"), []byte(`{"object":"chat.completion","choices":[{"index":0,"message":{"content":"x"}}]}`), plain("\xff"), false, false)
	f.Add([]byte(`{"choices":[{"index":0,"delta":{"content":"a","reasoning":"r"}}]}`), []byte(`{"choices":[{"index":0,"delta":{"content":"b","reasoning":"r"}}]}`), plain("c"), false, false)
	f.Add([]byte(`{"choices":[{"index":0,"delta":{"content":"NaN"}}],"v":NaN}`), []byte(`{"choices":[{"index":0,"delta":{"content":"Infinity"}}],"v":NaN}`), []byte(`{"choices":[{"index":0,"delta":{"content":"-Infinity"}}],"v":NaN}`), false, false)
	f.Add([]byte(`{"choices":[{"index":0,"delta":{"content":"a","reasoning_content":null}}]}`), []byte(`{"choices":[{"index":0,"delta":{"content":"b","reasoning_content":null}}]}`), []byte(`{"choices":[{"index":0,"delta":{"content":"c","reasoning_content":"r"}}]}`), false, false)
	f.Fuzz(func(t *testing.T, first, second, third []byte, keep, keepTop bool) {
		if bytes.ContainsAny(first, "\r\n") || bytes.ContainsAny(second, "\r\n") || bytes.ContainsAny(third, "\r\n") || topLevelKeyCount(first, second, third) > maxTopLevelFields {
			return
		}
		events := make([][]byte, 0, 6)
		for _, payload := range [][]byte{first, second, second, third, first, third} {
			events = append(events, append(append([]byte("data: "), payload...), "\n\n"...))
		}
		intent := LogprobIntent{Keep: keep, KeepTop: keepTop}

		templated, merged := foldEvents(t, events, intent, true), foldEvents(t, events, intent, false)

		if !bytes.Equal(templated, merged) {
			t.Fatalf("first %q second %q third %q intent %+v:\ntemplated %s\nmerged    %s", first, second, third, intent, templated, merged)
		}
	})
}

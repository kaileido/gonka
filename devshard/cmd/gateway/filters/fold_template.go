package filters

import (
	"bytes"
	stdjson "encoding/json"
	"slices"
	"unicode/utf8"
)

type contentTemplate struct {
	head, tail []byte
	delta      map[string]any
}

func newContentTemplate(payload []byte, decoded, merged map[string]any) *contentTemplate {
	choices, _ := decoded["choices"].([]any)
	if len(choices) != 1 {
		return nil
	}
	choice, _ := choices[0].(map[string]any)
	if _, indexed := numericIndex(choice["index"]); !indexed || !onlyDeltaGrows(choice) {
		return nil
	}
	delta, _ := choice["delta"].(map[string]any)
	if !onlyContentGrows(delta) {
		return nil
	}
	mergedChoices, _ := merged["choices"].([]any)
	if len(mergedChoices) >= maxIndexedElements {
		return nil
	}
	target, _ := elementAtIndex(mergedChoices, choice["index"])["delta"].(map[string]any)
	start, end, found := soleContentLiteral(payload)
	if target == nil || !found {
		return nil
	}
	return &contentTemplate{
		head:  append([]byte(nil), payload[:start]...),
		tail:  append([]byte(nil), payload[end:]...),
		delta: target,
	}
}

func onlyDeltaGrows(choice map[string]any) bool {
	for key, value := range choice {
		if key != "delta" && accumulatedChoiceFields[key] && value != nil {
			return false
		}
	}
	return true
}

func onlyContentGrows(delta map[string]any) bool {
	if _, isString := delta["content"].(string); !isString {
		return false
	}
	for key, value := range delta {
		if _, isString := value.(string); value != nil && !isString {
			return false
		}
		if key != "content" && accumulatedTextFields[key] && value != nil {
			return false
		}
	}
	return true
}

func soleContentLiteral(payload []byte) (start, end int, found bool) {
	contentKeys := 0
	unreadableKey := anyKeyMatches(payload, func(name []byte, valueAt int) bool {
		if string(name) == "content" {
			contentKeys++
			start = len(payload) - len(bytes.TrimLeft(payload[valueAt:], jsonWhitespace))
		}
		return false
	})
	if unreadableKey || contentKeys != 1 || start >= len(payload) || payload[start] != '"' {
		return 0, 0, false
	}
	return start, start + 1 + stringLiteralEnd(payload[start+1:]), true
}

func (t *contentTemplate) appendText(payload []byte) bool {
	text, matched := t.text(payload)
	if matched {
		t.delta["content"] = growText("content", t.delta["content"], text)
	}
	return matched
}

func (t *contentTemplate) text(payload []byte) (string, bool) {
	if len(payload) < len(t.head)+len(t.tail)+2 || !bytes.HasPrefix(payload, t.head) || !bytes.HasSuffix(payload, t.tail) {
		return "", false
	}
	literal := payload[len(t.head) : len(payload)-len(t.tail)]
	if literal[0] != '"' || literal[len(literal)-1] != '"' || 1+stringLiteralEnd(literal[1:]) != len(literal) {
		return "", false
	}
	inner := literal[1 : len(literal)-1]
	if bytes.IndexByte(inner, '\\') < 0 && utf8.Valid(inner) && !slices.ContainsFunc(inner, func(symbol byte) bool { return symbol < firstNonControlByte }) {
		return string(inner), true
	}
	var text string
	return text, stdjson.Unmarshal(literal, &text) == nil
}

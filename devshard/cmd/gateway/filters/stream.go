package filters

import (
	"bytes"
	stdjson "encoding/json"
	"errors"
	"fmt"
	"slices"
)

const (
	// MaxStreamCarryBytes bounds the unterminated tail held per stream. See README.md, "SSE framing".
	MaxStreamCarryBytes = 32 << 20

	// chunkObject tells a streaming client to read choices[].delta instead of choices[].message.
	chunkObject = "chat.completion.chunk"
)

var (
	sseLineSeparator      = []byte("\n")
	sseEventSeparator     = []byte("\n\n")
	sseEventSeparatorCRLF = []byte("\r\n\r\n")
	sseDataPrefix         = []byte("data: ")
	sseDoneMarker         = []byte("[DONE]")

	// sseDataParsePrefix deliberately omits the space sseDataPrefix emits. See README.md, "Two `data:` prefixes that must not be unified".
	sseDataParsePrefix = []byte("data:")

	// SSEDoneEvent is the terminator an SSE client reads until; without it the client waits out its own timeout.
	SSEDoneEvent = []byte("data: [DONE]\n\n")

	// NoResponseDataBody is the reply a non-streaming caller gets when the stream carried no payload.
	NoResponseDataBody = []byte(`{"error":{"message":"no response data"}}`)

	// TruncatedResponseBody replaces a fold past maxAssembledEvents; the prefix alone would look complete.
	TruncatedResponseBody = []byte(`{"error":{"message":"response exceeded the assembler's event budget"}}`)

	// ErrStreamCarryOverflow reports an unterminated SSE event larger than MaxStreamCarryBytes.
	ErrStreamCarryOverflow = errors.New("sse event exceeds carry limit")
	// ErrStreamTruncatedEvent reports a trailing partial event dropped at end of stream.
	ErrStreamTruncatedEvent = errors.New("sse stream ended mid-event")
)

// StreamRewriter strips the hidden fields from an SSE stream, emitting complete events only.
type StreamRewriter struct {
	intent    LogprobIntent
	keepUsage bool
	carry     []byte
	scanned   int
	failed    bool
}

// NewStreamRewriter drops the forced usage event unless keepUsage says the client asked for it.
func NewStreamRewriter(intent LogprobIntent, keepUsage bool) *StreamRewriter {
	return &StreamRewriter{intent: intent, keepUsage: keepUsage}
}

// Write returns every event chunk completes, rewritten; past MaxStreamCarryBytes the rewriter fails permanently.
func (r *StreamRewriter) Write(chunk []byte) ([]byte, error) {
	if r.failed {
		return nil, ErrStreamCarryOverflow
	}
	r.carry = append(r.carry, chunk...)
	var out bytes.Buffer
	eventStart, searchFrom := 0, r.scanned
	for {
		offset := indexEventEnd(r.carry[searchFrom:])
		if offset < 0 {
			break
		}
		eventEnd := searchFrom + offset
		rewritten, _ := rewriteEvent(r.carry[eventStart:eventEnd], r.intent, r.keepUsage)
		out.Write(rewritten)
		eventStart, searchFrom = eventEnd, eventEnd
	}
	r.carry = append(r.carry[:0], r.carry[eventStart:]...)
	r.scanned = max(0, len(r.carry)-len(sseEventSeparatorCRLF)+1)
	if len(r.carry) > MaxStreamCarryBytes {
		carried := len(r.carry)
		r.carry, r.scanned = nil, 0
		r.failed = true
		return out.Bytes(), fmt.Errorf("%w: %d bytes", ErrStreamCarryOverflow, carried)
	}
	return out.Bytes(), nil
}

// Close rewrites a well-formed trailing partial, and drops an unparseable one with ErrStreamTruncatedEvent.
func (r *StreamRewriter) Close() ([]byte, error) {
	carry := r.carry
	r.carry, r.scanned = nil, 0
	if r.failed {
		return nil, ErrStreamCarryOverflow
	}
	if len(carry) == 0 {
		return nil, nil
	}
	final, malformed := rewriteEvent(carry, r.intent, r.keepUsage)
	if malformed {
		return nil, ErrStreamTruncatedEvent
	}
	return final, nil
}

// rewriteEvent returns the event as the client must read it, deciding on the decoded payload and never on the host-controlled raw bytes. See README.md, "Rewriting an event".
func rewriteEvent(event []byte, intent LogprobIntent, keepUsage bool) (rewritten []byte, malformed bool) {
	dataLines, payload, held := eventPayload(event)
	if !held {
		return event, false
	}
	if dataLines == 1 && !intent.Keep && carriesNothingToRewrite(payload, keepUsage) {
		return event, false
	}
	return rewriteDecodedEvent(event, dataLines, payload, intent, keepUsage)
}

func rewriteDecodedEvent(event []byte, dataLines int, payload []byte, intent LogprobIntent, keepUsage bool) (rewritten []byte, malformed bool) {
	// A payload that opens as an object and does not parse would carry whatever it hides.
	unreadable := func() ([]byte, bool) {
		if bytes.HasPrefix(bytes.TrimLeft(payload, " \t"), []byte("{")) {
			return nil, true
		}
		return event, false
	}
	decoded, changed, ok := decodePayload(payload)
	if !ok {
		return unreadable()
	}
	changed = stripDecodedFields(decoded, intent) || changed
	if !keepUsage {
		dropped, emptied := dropUsage(decoded)
		if emptied {
			return nil, false
		}
		changed = dropped || changed
	}
	filtered := payload
	if changed {
		encoded, err := encodeCompact(decoded)
		if err != nil {
			return unreadable()
		}
		filtered = encoded
	}
	if carriesChoiceMessage(decoded) {
		if chunks, converted := completionAsChunks(filtered); converted {
			return chunks, false
		}
	}
	if !changed && dataLines == 1 {
		return event, false
	}
	return rebuildEvent(event, filtered), false
}

var rewriteTriggers = append(slices.Clone(clientStrippedFields), "message")

func carriesNothingToRewrite(payload []byte, keepUsage bool) bool {
	payload = bytes.Trim(payload, " \t\r\n")
	if len(payload) == 0 || payload[0] != '{' {
		return false
	}
	triggered := anyKey(payload, func(name []byte, _ int) bool {
		if !keepUsage && bytes.EqualFold(name, []byte("usage")) {
			return true
		}
		return slices.ContainsFunc(rewriteTriggers, func(trigger string) bool { return bytes.EqualFold(name, []byte(trigger)) })
	})
	return !triggered && stdjson.Valid(payload)
}

func anyKey(payload []byte, visit func(name []byte, valueAt int) bool) bool {
	for index := 0; index < len(payload); index++ {
		if payload[index] != '"' {
			continue
		}
		end, escaped := jsonStringEnd(payload, index)
		if colon, isKey := colonAfter(payload, end); isKey {
			name := payload[index+1 : end]
			if escaped {
				var unescaped string
				if stdjson.Unmarshal(payload[index:end+1], &unescaped) != nil {
					return true
				}
				name = []byte(unescaped)
			}
			if visit(name, colon+1) {
				return true
			}
		}
		index = end
	}
	return false
}

func jsonStringEnd(payload []byte, open int) (closing int, escaped bool) {
	for index := open + 1; index < len(payload); index++ {
		switch payload[index] {
		case '\\':
			escaped = true
			index++
		case '"':
			return index, escaped
		}
	}
	return len(payload) - 1, escaped
}

func colonAfter(payload []byte, closing int) (int, bool) {
	for index := closing + 1; index < len(payload); index++ {
		switch payload[index] {
		case ' ', '\t', '\r', '\n':
			continue
		case ':':
			return index, true
		default:
			return 0, false
		}
	}
	return 0, false
}

// chunkHousekeepingFields are what the final usage event carries besides its usage.
var chunkHousekeepingFields = map[string]bool{
	"id": true, "object": true, "created": true, "model": true, "system_fingerprint": true,
	"service_tier": true, "choices": true,
}

// onlyHousekeepingLeft: testing for empty choices instead would delete a host's error event, which carries none either.
func onlyHousekeepingLeft(decoded map[string]any) bool {
	for field := range decoded {
		if !chunkHousekeepingFields[field] {
			return false
		}
	}
	choices, _ := decoded["choices"].([]any)
	return len(choices) == 0
}

// dropUsage deletes a usage the client never asked for from what the strip decoded, emptied for an event left with none.
func dropUsage(decoded any) (dropped, emptied bool) {
	event, isObject := decoded.(map[string]any)
	if !isObject {
		return false, false
	}
	if _, held := event["usage"]; !held {
		return false, false
	}
	delete(event, "usage")
	return true, onlyHousekeepingLeft(event)
}

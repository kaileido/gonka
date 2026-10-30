package peerload

import (
	"fmt"
	"math"
	"time"
	"unicode/utf8"

	"google.golang.org/protobuf/encoding/protowire"
)

// Field numbers of the wire schema. See docs/peerload.md, "Wire schema".
const (
	envelopeFieldTopic        protowire.Number = 1
	envelopeFieldTopicVersion protowire.Number = 2
	envelopeFieldOrigin       protowire.Number = 3
	envelopeFieldSeq          protowire.Number = 4
	envelopeFieldIssuedAtMS   protowire.Number = 5
	envelopeFieldPayload      protowire.Number = 8
	envelopeFieldInstance     protowire.Number = 9
)

type peerEnvelope struct {
	Topic        string
	TopicVersion uint32
	Origin       string
	Instance     string
	Seq          uint64
	IssuedAtMS   int64
	Payload      []byte
}

func (e peerEnvelope) marshal() []byte {
	encoded := make([]byte, 0, len(e.Payload)+envelopeOverhead)
	encoded = protowire.AppendTag(encoded, envelopeFieldTopic, protowire.BytesType)
	encoded = protowire.AppendString(encoded, e.Topic)
	encoded = appendVarintField(encoded, envelopeFieldTopicVersion, uint64(e.TopicVersion))
	encoded = protowire.AppendTag(encoded, envelopeFieldOrigin, protowire.BytesType)
	encoded = protowire.AppendString(encoded, e.Origin)
	encoded = appendVarintField(encoded, envelopeFieldSeq, e.Seq)
	encoded = appendVarintField(encoded, envelopeFieldIssuedAtMS, uint64(e.IssuedAtMS))
	encoded = protowire.AppendTag(encoded, envelopeFieldPayload, protowire.BytesType)
	encoded = protowire.AppendBytes(encoded, e.Payload)
	encoded = protowire.AppendTag(encoded, envelopeFieldInstance, protowire.BytesType)
	return protowire.AppendString(encoded, e.Instance)
}

func decodeEnvelope(raw []byte) (peerEnvelope, error) {
	var envelope peerEnvelope
	err := eachField(raw, func(field wireField) error {
		switch field.number {
		case envelopeFieldTopic:
			return field.readString(&envelope.Topic)
		case envelopeFieldTopicVersion:
			return field.readUint32(&envelope.TopicVersion)
		case envelopeFieldOrigin:
			return field.readString(&envelope.Origin)
		case envelopeFieldSeq:
			return field.readUint64(&envelope.Seq)
		case envelopeFieldIssuedAtMS:
			var issued uint64
			if err := field.readUint64(&issued); err != nil {
				return err
			}
			envelope.IssuedAtMS = int64(issued)
		case envelopeFieldPayload:
			if field.wireType != protowire.BytesType {
				return errMalformed
			}
			envelope.Payload = field.bytes
		case envelopeFieldInstance:
			return field.readString(&envelope.Instance)
		}
		return nil
	})
	return envelope, err
}

func checkEnvelope(envelope peerEnvelope, topic peerTopic, from PeerID, previousSeq uint64, now time.Time) error {
	if envelope.Topic != topic.Name() || envelope.TopicVersion != topic.Version() {
		return fmt.Errorf("%w: wrong topic from %s", errEnvelope, from)
	}
	if envelope.Origin != from.Address || envelope.Instance != from.Instance {
		return fmt.Errorf("%w: wrong origin on the connection of %s", errEnvelope, from)
	}
	issued := time.UnixMilli(envelope.IssuedAtMS)
	if now.Sub(issued) > topic.MaxAge() || issued.Sub(now) > topic.MaxAge() {
		return fmt.Errorf("%w: issued %s, now %s", errStaleEnvelope, issued, now)
	}
	if envelope.Seq <= previousSeq {
		return fmt.Errorf("%w: %d after %d", errReplay, envelope.Seq, previousSeq)
	}
	return nil
}

type wireField struct {
	number   protowire.Number
	wireType protowire.Type
	varint   uint64
	fixed32  uint32
	bytes    []byte
}

func (f wireField) readString(into *string) error {
	// A peer's string reaches a metric label, and an invalid label value panics the scrape.
	if f.wireType != protowire.BytesType || !utf8.Valid(f.bytes) {
		return errMalformed
	}
	*into = string(f.bytes)
	return nil
}

func (f wireField) readUint64(into *uint64) error {
	if f.wireType != protowire.VarintType {
		return errMalformed
	}
	*into = f.varint
	return nil
}

func (f wireField) readUint32(into *uint32) error {
	if f.wireType != protowire.VarintType || f.varint > math.MaxUint32 {
		return errMalformed
	}
	*into = uint32(f.varint)
	return nil
}

func (f wireField) readFloat(into *float32) error {
	if f.wireType != protowire.Fixed32Type {
		return errMalformed
	}
	*into = math.Float32frombits(f.fixed32)
	return nil
}

func appendVarintField(encoded []byte, number protowire.Number, value uint64) []byte {
	encoded = protowire.AppendTag(encoded, number, protowire.VarintType)
	return protowire.AppendVarint(encoded, value)
}

func eachField(raw []byte, visit func(wireField) error) error {
	for len(raw) > 0 {
		number, wireType, consumed := protowire.ConsumeTag(raw)
		if consumed < 0 {
			return errMalformed
		}
		raw = raw[consumed:]
		field := wireField{number: number, wireType: wireType}
		switch wireType {
		case protowire.VarintType:
			field.varint, consumed = protowire.ConsumeVarint(raw)
		case protowire.BytesType:
			field.bytes, consumed = protowire.ConsumeBytes(raw)
		case protowire.Fixed32Type:
			field.fixed32, consumed = protowire.ConsumeFixed32(raw)
		case protowire.Fixed64Type:
			_, consumed = protowire.ConsumeFixed64(raw)
		default:
			// A group is the only recursive wire form, and a peer's bytes never reach a recursive parser.
			return errMalformed
		}
		if consumed < 0 {
			return errMalformed
		}
		raw = raw[consumed:]
		if err := visit(field); err != nil {
			return err
		}
	}
	return nil
}

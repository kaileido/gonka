package peerload

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
)

// Field numbers of the wire schema. See docs/peerload.md, "Wire schema".
const (
	payloadFieldIntervalMS protowire.Number = 1
	payloadFieldModels     protowire.Number = 2
	payloadFieldHosts      protowire.Number = 3

	hostFieldParticipant          protowire.Number = 1
	hostFieldModel                protowire.Number = 2
	hostFieldInflightRequests     protowire.Number = 3
	hostFieldInflightInputTokens  protowire.Number = 4
	hostFieldInflightOutputTokens protowire.Number = 5
	hostFieldCutoff               protowire.Number = 6
	hostFieldEjected              protowire.Number = 7
	hostFieldPressureFirstContent protowire.Number = 8
	hostFieldPressureDecode       protowire.Number = 9
)

// HostLoad is one row of the load topic, for one {participant, model}.
type HostLoad struct {
	Participant          string
	Model                string
	InflightRequests     uint32
	InflightInputTokens  uint32
	InflightOutputTokens uint32
	Cutoff               Cutoff
	Ejected              bool
	PressureFirstContent float32
	PressureDecode       float32
}

// LoadSample is the sender's own raw state for one {participant, model}, before rounding.
type LoadSample struct {
	Participant          string
	Model                string
	InflightRequests     int64
	InflightInputTokens  int64
	InflightOutputTokens int64
	Cutoff               Cutoff
	Ejected              bool
	PressureFirstContent float64
	PressureDecode       float64
}

// Rounding is the multiple the in-flight counts round up to, so a peer sees less of the sender's volume.
type Rounding struct {
	Requests uint32
	Tokens   uint32
}

// DefaultRounding is the rounding unless configured otherwise.
func DefaultRounding() Rounding {
	return Rounding{Requests: defaultRoundRequests, Tokens: defaultRoundTokens}
}

type loadTopic struct {
	samples  func() []LoadSample
	rounding Rounding
	view     *View
}

func newLoadTopic(samples func() []LoadSample, rounding Rounding, view *View) *loadTopic {
	return &loadTopic{samples: samples, rounding: rounding, view: view}
}

func (t *loadTopic) Name() string            { return loadTopicName }
func (t *loadTopic) Version() uint32         { return loadTopicVersion }
func (t *loadTopic) MaxBytes() int           { return loadMaxBytes }
func (t *loadTopic) Interval() time.Duration { return loadInterval }
func (t *loadTopic) MaxAge() time.Duration   { return loadMaxAge }

// Build encodes the busiest rows that fit the row, model and byte caps.
func (t *loadTopic) Build(time.Time) ([]byte, error) {
	samples := t.samples()
	rows := make([]HostLoad, 0, len(samples))
	for _, sample := range samples {
		rows = append(rows, roundedRow(sample, t.rounding))
	}
	return encodeLoadPayload(rows), nil
}

// Accept decodes a peer's rows; a payload past any cap is refused whole.
func (t *loadTopic) Accept(from PeerID, envelope peerEnvelope, now time.Time) error {
	rows, err := decodeLoadPayload(envelope.Payload)
	if err != nil {
		return fmt.Errorf("load payload of %s: %w", from, err)
	}
	t.view.Store(from, rows, min(envelope.IssuedAtMS, now.UnixMilli()))
	return nil
}

func roundedRow(sample LoadSample, rounding Rounding) HostLoad {
	return HostLoad{
		Participant:          sample.Participant,
		Model:                sample.Model,
		InflightRequests:     roundUp(sample.InflightRequests, rounding.Requests),
		InflightInputTokens:  roundUp(sample.InflightInputTokens, rounding.Tokens),
		InflightOutputTokens: roundUp(sample.InflightOutputTokens, rounding.Tokens),
		Cutoff:               sample.Cutoff,
		Ejected:              sample.Ejected,
		PressureFirstContent: float32(sample.PressureFirstContent),
		PressureDecode:       float32(sample.PressureDecode),
	}
}

func roundUp(value int64, multiple uint32) uint32 {
	if value <= 0 {
		return 0
	}
	step := max(uint64(multiple), 1)
	limit := uint64(math.MaxUint32) / step * step
	if uint64(value) >= limit {
		return uint32(limit)
	}
	return uint32((uint64(value) + step - 1) / step * step)
}

func encodeLoadPayload(rows []HostLoad) []byte {
	sorted := slices.Clone(rows)
	slices.SortFunc(sorted, func(a, b HostLoad) int {
		return cmp.Or(cmp.Compare(b.InflightRequests, a.InflightRequests), strings.Compare(a.Participant, b.Participant), strings.Compare(a.Model, b.Model))
	})
	budget := loadMaxBytes - envelopeOverhead
	models := make([]string, 0, maxLoadModels)
	encoded := appendVarintField(nil, payloadFieldIntervalMS, uint64(loadInterval.Milliseconds()))
	var hosts []byte
	written := 0
	for _, row := range sorted {
		if written == maxLoadRows || len(row.Participant) > maxLoadStringBytes || len(row.Model) > maxLoadStringBytes {
			continue
		}
		position := slices.Index(models, row.Model)
		isNewModel := position < 0
		modelBytes := 0
		if isNewModel {
			if len(models) == maxLoadModels {
				continue
			}
			position = len(models)
			modelBytes = protowire.SizeTag(payloadFieldModels) + protowire.SizeBytes(len(row.Model))
		}
		host := appendHostLoad(nil, row, uint64(position))
		hostBytes := protowire.SizeTag(payloadFieldHosts) + protowire.SizeBytes(len(host))
		if len(encoded)+len(hosts)+modelBytes+hostBytes > budget {
			continue
		}
		if isNewModel {
			models = append(models, row.Model)
			encoded = protowire.AppendTag(encoded, payloadFieldModels, protowire.BytesType)
			encoded = protowire.AppendString(encoded, row.Model)
		}
		hosts = protowire.AppendTag(hosts, payloadFieldHosts, protowire.BytesType)
		hosts = protowire.AppendBytes(hosts, host)
		written++
	}
	return append(encoded, hosts...)
}

func appendHostLoad(encoded []byte, row HostLoad, modelIndex uint64) []byte {
	encoded = protowire.AppendTag(encoded, hostFieldParticipant, protowire.BytesType)
	encoded = protowire.AppendString(encoded, row.Participant)
	encoded = appendVarintField(encoded, hostFieldModel, modelIndex)
	encoded = appendVarintField(encoded, hostFieldInflightRequests, uint64(row.InflightRequests))
	encoded = appendVarintField(encoded, hostFieldInflightInputTokens, uint64(row.InflightInputTokens))
	encoded = appendVarintField(encoded, hostFieldInflightOutputTokens, uint64(row.InflightOutputTokens))
	encoded = appendVarintField(encoded, hostFieldCutoff, uint64(row.Cutoff))
	encoded = appendVarintField(encoded, hostFieldEjected, protowire.EncodeBool(row.Ejected))
	encoded = appendFloatField(encoded, hostFieldPressureFirstContent, row.PressureFirstContent)
	return appendFloatField(encoded, hostFieldPressureDecode, row.PressureDecode)
}

func appendFloatField(encoded []byte, number protowire.Number, value float32) []byte {
	encoded = protowire.AppendTag(encoded, number, protowire.Fixed32Type)
	return protowire.AppendFixed32(encoded, math.Float32bits(value))
}

type rawHostLoad struct {
	row        HostLoad
	modelIndex uint32
}

func decodeLoadPayload(raw []byte) ([]HostLoad, error) {
	var models []string
	var hosts []rawHostLoad
	err := eachField(raw, func(field wireField) error {
		switch field.number {
		case payloadFieldModels:
			var model string
			if err := field.readString(&model); err != nil {
				return err
			}
			if len(models) == maxLoadModels || len(model) > maxLoadStringBytes {
				return errTooLarge
			}
			models = append(models, model)
		case payloadFieldHosts:
			if field.wireType != protowire.BytesType {
				return errMalformed
			}
			if len(hosts) == maxLoadRows {
				return errTooLarge
			}
			host, err := decodeHostLoad(field.bytes)
			if err != nil {
				return err
			}
			hosts = append(hosts, host)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	rows := make([]HostLoad, 0, len(hosts))
	for _, host := range hosts {
		if int(host.modelIndex) >= len(models) {
			return nil, errMalformed
		}
		host.row.Model = models[host.modelIndex]
		rows = append(rows, host.row)
	}
	return rows, nil
}

func decodeHostLoad(raw []byte) (rawHostLoad, error) {
	var host rawHostLoad
	err := eachField(raw, func(field wireField) error {
		switch field.number {
		case hostFieldParticipant:
			if err := field.readString(&host.row.Participant); err != nil {
				return err
			}
			if len(host.row.Participant) > maxLoadStringBytes {
				return errTooLarge
			}
		case hostFieldModel:
			return field.readUint32(&host.modelIndex)
		case hostFieldInflightRequests:
			return field.readUint32(&host.row.InflightRequests)
		case hostFieldInflightInputTokens:
			return field.readUint32(&host.row.InflightInputTokens)
		case hostFieldInflightOutputTokens:
			return field.readUint32(&host.row.InflightOutputTokens)
		case hostFieldCutoff:
			var cutoff uint32
			if err := field.readUint32(&cutoff); err != nil {
				return err
			}
			host.row.Cutoff = Cutoff(cutoff)
		case hostFieldEjected:
			var ejected uint64
			if err := field.readUint64(&ejected); err != nil {
				return err
			}
			host.row.Ejected = protowire.DecodeBool(ejected)
		case hostFieldPressureFirstContent:
			return field.readFloat(&host.row.PressureFirstContent)
		case hostFieldPressureDecode:
			return field.readFloat(&host.row.PressureDecode)
		}
		return nil
	})
	return host, err
}

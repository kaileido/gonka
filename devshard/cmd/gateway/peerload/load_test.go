package peerload

import (
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
)

func sampleRows(count int) []HostLoad {
	rows := make([]HostLoad, 0, count)
	for index := range count {
		rows = append(rows, HostLoad{Participant: fmt.Sprintf("gonka1host%04d", index), Model: testModel, InflightRequests: uint32(index)})
	}
	return rows
}

// Test flow:
//  1. Round values around the request and token multiples and past the wire's range.
//  2. Assert each rounds up, zero and negative stay zero, and the top saturates.
func TestRoundUpGoesToTheNextMultipleAndSaturates(t *testing.T) {
	cases := []struct {
		value    int64
		multiple uint32
		want     uint32
	}{
		{0, 2, 0},
		{-5, 2, 0},
		{1, 2, 2},
		{2, 2, 2},
		{3, 2, 4},
		{1, 2048, 2048},
		{2048, 2048, 2048},
		{2049, 2048, 4096},
		{7, 0, 7},
		{math.MaxInt64, 2048, math.MaxUint32 / 2048 * 2048},
	}
	for _, testCase := range cases {
		if got := roundUp(testCase.value, testCase.multiple); got != testCase.want {
			t.Errorf("roundUp(%d, %d) = %d, want %d", testCase.value, testCase.multiple, got, testCase.want)
		}
	}
}

// Test flow:
//  1. Build the topic from one raw sample with the default rounding.
//  2. Decode what it serves.
//  3. Assert every field arrives, the in-flight counts rounded up.
func TestLoadTopicServesEveryFieldRoundedUp(t *testing.T) {
	topic := newLoadTopic(func() []LoadSample {
		return []LoadSample{{Participant: testHost, Model: testModel, InflightRequests: 3, InflightInputTokens: 1, InflightOutputTokens: 2049, Cutoff: CutoffHalfOpen, Ejected: true, PressureFirstContent: 1.5, PressureDecode: 2.5}}
	}, DefaultRounding(), nil)

	payload, err := topic.Build(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := decodeLoadPayload(payload)
	if err != nil {
		t.Fatal(err)
	}

	want := HostLoad{Participant: testHost, Model: testModel, InflightRequests: 4, InflightInputTokens: 2048, InflightOutputTokens: 4096, Cutoff: CutoffHalfOpen, Ejected: true, PressureFirstContent: 1.5, PressureDecode: 2.5}
	if len(rows) != 1 || rows[0] != want {
		t.Fatalf("rows = %+v, want %+v", rows, want)
	}
}

// Test flow:
//  1. Encode 600 rows.
//  2. Decode them.
//  3. Assert 512 arrive and the busiest ones are the ones kept.
func TestEncodeKeepsTheBusiestRowsUpToTheRowCap(t *testing.T) {
	rows, err := decodeLoadPayload(encodeLoadPayload(sampleRows(600)))
	if err != nil {
		t.Fatal(err)
	}

	if len(rows) != maxLoadRows {
		t.Fatalf("rows = %d, want %d", len(rows), maxLoadRows)
	}
	if rows[0].InflightRequests != 599 || rows[maxLoadRows-1].InflightRequests != 88 {
		t.Fatalf("kept %d..%d, want 599..88", rows[0].InflightRequests, rows[maxLoadRows-1].InflightRequests)
	}
}

// Test flow:
//  1. Hand-encode a payload with 513 rows, one with 33 models, and strings past the 128-byte cap.
//  2. Decode each.
//  3. Assert each is refused whole as too large.
func TestDecodeRefusesAPayloadPastAnyCap(t *testing.T) {
	appendModel := func(encoded []byte, model string) []byte {
		encoded = protowire.AppendTag(encoded, payloadFieldModels, protowire.BytesType)
		return protowire.AppendString(encoded, model)
	}
	appendHost := func(encoded []byte, participant string) []byte {
		encoded = protowire.AppendTag(encoded, payloadFieldHosts, protowire.BytesType)
		return protowire.AppendBytes(encoded, appendHostLoad(nil, HostLoad{Participant: participant}, 0))
	}
	tooManyRows := appendModel(nil, testModel)
	for range maxLoadRows + 1 {
		tooManyRows = appendHost(tooManyRows, testHost)
	}
	var tooManyModels []byte
	for index := range maxLoadModels + 1 {
		tooManyModels = appendModel(tooManyModels, fmt.Sprint(index))
	}
	cases := map[string][]byte{
		"rows":             tooManyRows,
		"models":           tooManyModels,
		"long model":       appendModel(nil, strings.Repeat("m", maxLoadStringBytes+1)),
		"long participant": appendHost(appendModel(nil, testModel), strings.Repeat("p", maxLoadStringBytes+1)),
	}
	for name, encoded := range cases {
		if _, err := decodeLoadPayload(encoded); !errors.Is(err, errTooLarge) {
			t.Errorf("decodeLoadPayload(%s) = %v, want errTooLarge", name, err)
		}
	}
}

// Test flow:
//  1. Encode 512 rows with the longest allowed participant names and 40 models with the longest names.
//  2. Wrap them in an envelope.
//  3. Assert the envelope fits the topic's byte cap and at most 32 models are named.
func TestLargestPayloadFitsTheEnvelopeByteCap(t *testing.T) {
	rows := make([]HostLoad, 0, maxLoadRows)
	for index := range maxLoadRows {
		suffix := fmt.Sprintf("%04d", index)
		rows = append(rows, HostLoad{
			Participant: strings.Repeat("p", maxLoadStringBytes-len(suffix)) + suffix,
			Model:       strings.Repeat("m", maxLoadStringBytes-2) + fmt.Sprintf("%02d", index%40),
		})
	}
	payload := encodeLoadPayload(rows)
	encoded := peerEnvelope{Topic: loadTopicName, TopicVersion: loadTopicVersion, Origin: strings.Repeat("a", 64), Instance: strings.Repeat("i", instanceIDLimit), Seq: math.MaxUint64, IssuedAtMS: math.MaxInt64, Payload: payload}.marshal()

	if len(encoded) > loadMaxBytes {
		t.Fatalf("envelope = %d bytes, cap %d", len(encoded), loadMaxBytes)
	}
	decoded, err := decodeLoadPayload(payload)
	if err != nil {
		t.Fatal(err)
	}
	models := map[string]bool{}
	for _, row := range decoded {
		models[row.Model] = true
	}
	if len(models) > maxLoadModels {
		t.Fatalf("models = %d, cap %d", len(models), maxLoadModels)
	}
}

// Test flow:
//  1. Hand a body at the cap, and one byte past it, to the capped reader.
//  2. Assert the first is read and the second refused.
func TestReadCappedRefusesAResponsePastTheCap(t *testing.T) {
	if read, err := readCapped(strings.NewReader(strings.Repeat("x", 8)), 8); err != nil || len(read) != 8 {
		t.Fatalf("readCapped(at cap) = %d bytes, %v", len(read), err)
	}
	if _, err := readCapped(strings.NewReader(strings.Repeat("x", 9)), 8); !errors.Is(err, errTooLarge) {
		t.Fatalf("readCapped(past cap) = %v, want errTooLarge", err)
	}
}

// Test flow:
//  1. Encode an envelope and decode it.
//  2. Decode a truncated copy, a group, a wrong wire type and an unknown field.
//  3. Assert the round trip, the refusals and the skip.
func TestEnvelopeDecoderIsFlatAndStrict(t *testing.T) {
	envelope := peerEnvelope{Topic: loadTopicName, TopicVersion: loadTopicVersion, Origin: secondAddress, Instance: "other", Seq: 9, IssuedAtMS: 1234, Payload: []byte{1, 2}}
	encoded := envelope.marshal()
	decoded, err := decodeEnvelope(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Topic != envelope.Topic || decoded.TopicVersion != envelope.TopicVersion || decoded.Origin != envelope.Origin || decoded.Instance != envelope.Instance || decoded.Seq != envelope.Seq || decoded.IssuedAtMS != envelope.IssuedAtMS || !slices.Equal(decoded.Payload, envelope.Payload) {
		t.Fatalf("decoded = %+v, want %+v", decoded, envelope)
	}

	group := protowire.AppendTag(nil, envelopeFieldTopic, protowire.StartGroupType)
	wrongType := protowire.AppendTag(nil, envelopeFieldTopic, protowire.VarintType)
	wrongType = protowire.AppendVarint(wrongType, 1)
	for name, malformed := range map[string][]byte{"truncated": encoded[:len(encoded)-1], "group": group, "wrong type": wrongType} {
		if _, err := decodeEnvelope(malformed); !errors.Is(err, errMalformed) {
			t.Errorf("DecodeEnvelope(%s) = %v, want errMalformed", name, err)
		}
	}

	unknown := protowire.AppendTag(slices.Clone(encoded), 100, protowire.Fixed64Type)
	unknown = protowire.AppendFixed64(unknown, 1)
	if _, err := decodeEnvelope(unknown); err != nil {
		t.Errorf("DecodeEnvelope(unknown field) = %v", err)
	}
}

// Test flow:
//  1. Start from a valid envelope for one connection.
//  2. Change one field at a time: topic, version, origin, instance, age each way, sequence.
//  3. Assert each change gets its own refusal, and the valid one passes.
func TestCheckEnvelopeBindsTheResponseToTheConnectionAndTheClock(t *testing.T) {
	now := newFakeClock().now()
	from := PeerID{Address: secondAddress, Instance: "other"}
	topic := newLoadTopic(nil, DefaultRounding(), nil)
	valid := peerEnvelope{Topic: loadTopicName, TopicVersion: loadTopicVersion, Origin: from.Address, Instance: from.Instance, Seq: 10, IssuedAtMS: now.UnixMilli()}
	if err := checkEnvelope(valid, topic, from, 9, now); err != nil {
		t.Fatalf("CheckEnvelope(valid) = %v", err)
	}
	cases := []struct {
		name   string
		change func(*peerEnvelope)
		want   error
	}{
		{"topic", func(e *peerEnvelope) { e.Topic = "other" }, errEnvelope},
		{"version", func(e *peerEnvelope) { e.TopicVersion = 2 }, errEnvelope},
		{"origin", func(e *peerEnvelope) { e.Origin = thirdAddress }, errEnvelope},
		{"instance", func(e *peerEnvelope) { e.Instance = "else" }, errEnvelope},
		{"old", func(e *peerEnvelope) { e.IssuedAtMS = now.Add(-loadMaxAge - time.Millisecond).UnixMilli() }, errStaleEnvelope},
		{"future", func(e *peerEnvelope) { e.IssuedAtMS = now.Add(loadMaxAge + time.Millisecond).UnixMilli() }, errStaleEnvelope},
		{"replay", func(e *peerEnvelope) { e.Seq = 9 }, errReplay},
		{"unset", func(e *peerEnvelope) { e.Seq = 0 }, errReplay},
	}
	for _, testCase := range cases {
		changed := valid
		testCase.change(&changed)
		if err := checkEnvelope(changed, topic, from, 9, now); !errors.Is(err, testCase.want) {
			t.Errorf("CheckEnvelope(%s) = %v, want %v", testCase.name, err, testCase.want)
		}
	}
}

type oversizedTopic struct{ *loadTopic }

func (oversizedTopic) Name() string  { return "oversized" }
func (oversizedTopic) MaxBytes() int { return 16 }
func (oversizedTopic) Build(time.Time) ([]byte, error) {
	return []byte(strings.Repeat("x", 64)), nil
}

func newServerFixture(t *testing.T) (*peerServer, *verifierFixture) {
	t.Helper()
	fixture := newVerifierFixture(t)
	load := newLoadTopic(func() []LoadSample { return nil }, DefaultRounding(), nil)
	server := newServer(fixture.members.self.ID, fixture.verifier, fixture.selfID, []peerTopic{load, oversizedTopic{load}}, fixture.clock.now)
	return server, fixture
}

func serve(server *peerServer, request *http.Request) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	return recorder
}

// Test flow:
//  1. Request an unknown topic on a verified connection.
//  2. Assert 404, the unknown_topic header and an empty body.
func TestUnknownTopicAnswers404WithTheErrorHeaderAndNoBody(t *testing.T) {
	server, fixture := newServerFixture(t)
	request := httptest.NewRequest(http.MethodGet, peerPathPrefix+"absent", http.NoBody)
	state := stateOf(fixture.peer)
	request.TLS = &state

	recorder := serve(server, request)

	if recorder.Code != http.StatusNotFound || recorder.Header().Get(headerPeerError) != peerErrorUnknownTopic || recorder.Body.Len() != 0 {
		t.Fatalf("status %d, header %q, %d body bytes", recorder.Code, recorder.Header().Get(headerPeerError), recorder.Body.Len())
	}
}

// Test flow:
//  1. Request the load topic with no TLS state, with a stranger's certificate, and with a pinned peer since removed from the allowlist.
//  2. Assert each is refused with 403 before any topic runs.
func TestRequestsOffAVerifiedConnectionAreRefused(t *testing.T) {
	server, fixture := newServerFixture(t)
	stranger := stateOf(newIdentity(t, PeerID{Address: thirdAddress, Instance: "stranger"}))
	removed := stateOf(fixture.peer)

	plain := httptest.NewRequest(http.MethodGet, peerPathPrefix+loadTopicName, http.NoBody)
	if got := serve(server, plain).Code; got != http.StatusForbidden {
		t.Errorf("no TLS: status %d, want 403", got)
	}
	fromStranger := httptest.NewRequest(http.MethodGet, peerPathPrefix+loadTopicName, http.NoBody)
	fromStranger.TLS = &stranger
	if got := serve(server, fromStranger).Code; got != http.StatusForbidden {
		t.Errorf("stranger: status %d, want 403", got)
	}
	fixture.members.ObserveChain([]string{firstAddress}, testHeight)
	fromRemoved := httptest.NewRequest(http.MethodGet, peerPathPrefix+loadTopicName, http.NoBody)
	fromRemoved.TLS = &removed
	if got := serve(server, fromRemoved).Code; got != http.StatusForbidden {
		t.Errorf("removed: status %d, want 403", got)
	}
}

// Test flow:
//  1. On a verified connection, send a GET with a body, a POST, and a topic whose response outgrows its cap.
//  2. Assert 400, 405 and 500, with nothing served past the cap.
func TestRequestShapeAndResponseCapAreEnforced(t *testing.T) {
	server, fixture := newServerFixture(t)
	state := stateOf(fixture.peer)
	cases := []struct {
		name   string
		method string
		path   string
		body   io.Reader
		want   int
	}{
		{"body", http.MethodGet, peerPathPrefix + loadTopicName, strings.NewReader("x"), http.StatusBadRequest},
		{"oversized", http.MethodGet, peerPathPrefix + "oversized", http.NoBody, http.StatusInternalServerError},
	}
	for _, testCase := range cases {
		request := httptest.NewRequest(testCase.method, testCase.path, testCase.body)
		request.TLS = &state
		recorder := serve(server, request)
		if recorder.Code != testCase.want || recorder.Body.Len() != 0 {
			t.Errorf("%s: status %d with %d body bytes, want %d and none", testCase.name, recorder.Code, recorder.Body.Len(), testCase.want)
		}
	}
	post := httptest.NewRequest(http.MethodPost, peerPathPrefix+loadTopicName, http.NoBody)
	post.TLS = &state
	if got := serve(server, post).Code; got != http.StatusMethodNotAllowed {
		t.Errorf("post: status %d, want 405", got)
	}
}

// Test flow:
//  1. Decode a payload whose model and whose participant are not valid UTF-8.
//  2. Assert each is refused, so it can never reach a metric label.
func TestDecodeRefusesInvalidUTF8(t *testing.T) {
	invalid := string([]byte{0xff, 0xfe})
	model := protowire.AppendTag(nil, payloadFieldModels, protowire.BytesType)
	model = protowire.AppendString(model, invalid)
	host := protowire.AppendTag(nil, payloadFieldHosts, protowire.BytesType)
	host = protowire.AppendBytes(host, appendHostLoad(nil, HostLoad{Participant: invalid}, 0))
	for name, encoded := range map[string][]byte{"model": model, "participant": host} {
		if _, err := decodeLoadPayload(encoded); !errors.Is(err, errMalformed) {
			t.Errorf("decodeLoadPayload(%s) = %v, want errMalformed", name, err)
		}
	}
}

// Test flow:
//  1. Fuzz the envelope decoder and, through its payload, the load decoder with arbitrary bytes.
//  2. Assert neither panics, and that any accepted load payload stays within the row cap.
func FuzzDecoders(f *testing.F) {
	f.Add(peerEnvelope{Topic: loadTopicName, Payload: encodeLoadPayload(sampleRows(2))}.marshal())
	f.Add(encodeLoadPayload(sampleRows(2)))
	f.Add([]byte{0x0b})
	f.Fuzz(func(t *testing.T, raw []byte) {
		if envelope, err := decodeEnvelope(raw); err == nil {
			_, _ = decodeLoadPayload(envelope.Payload)
		}
		if rows, err := decodeLoadPayload(raw); err == nil && len(rows) > maxLoadRows {
			t.Fatalf("decoded %d rows past the cap", len(rows))
		}
	})
}

// Test flow:
//  1. Send HEAD and POST to the topic on a verified connection.
//  2. Assert each answers 405 and names GET.
func TestAnyMethodButGetAnswers405(t *testing.T) {
	server, fixture := newServerFixture(t)
	state := stateOf(fixture.peer)
	for _, method := range []string{http.MethodHead, http.MethodPost} {
		request := httptest.NewRequest(method, peerPathPrefix+loadTopicName, http.NoBody)
		request.TLS = &state
		response := serve(server, request)
		if response.Code != http.StatusMethodNotAllowed || response.Header().Get(headerAllow) != http.MethodGet {
			t.Errorf("%s = %d, Allow %q; want 405 naming GET", method, response.Code, response.Header().Get(headerAllow))
		}
	}
}

// Test flow:
//  1. Encode a payload with one model and a row whose model index is past the table.
//  2. Assert it is refused as malformed, without a panic.
func TestDecodeRefusesAModelIndexOutsideTheTable(t *testing.T) {
	payload := protowire.AppendTag(nil, payloadFieldModels, protowire.BytesType)
	payload = protowire.AppendString(payload, testModel)
	payload = protowire.AppendTag(payload, payloadFieldHosts, protowire.BytesType)
	payload = protowire.AppendBytes(payload, appendHostLoad(nil, HostLoad{Participant: testHost}, 5))

	if _, err := decodeLoadPayload(payload); !errors.Is(err, errMalformed) {
		t.Fatalf("decodeLoadPayload() = %v, want errMalformed", err)
	}
}

// Test flow:
//  1. Encode a payload whose only content is a group under a field number nobody reads.
//  2. Assert it is refused as malformed.
func TestDecodeRefusesAGroupUnderAnUnknownField(t *testing.T) {
	const unknownField protowire.Number = 99
	payload := protowire.AppendTag(nil, unknownField, protowire.StartGroupType)
	payload = protowire.AppendTag(payload, unknownField, protowire.EndGroupType)

	if _, err := decodeLoadPayload(payload); !errors.Is(err, errMalformed) {
		t.Fatalf("decodeLoadPayload() = %v, want errMalformed", err)
	}
}

// Test flow:
//  1. Serve the topic twice with the clock standing still, then once with the clock moved back.
//  2. Assert the sequence strictly increases each time.
func TestServedSequenceIncreasesWhenTheClockDoesNot(t *testing.T) {
	server, fixture := newServerFixture(t)
	state := stateOf(fixture.peer)
	var previous uint64
	for step, move := range []time.Duration{0, 0, -time.Minute} {
		fixture.clock.advance(move)
		request := httptest.NewRequest(http.MethodGet, peerPathPrefix+loadTopicName, http.NoBody)
		request.TLS = &state
		envelope, err := decodeEnvelope(serve(server, request).Body.Bytes())
		if err != nil {
			t.Fatal(err)
		}
		if envelope.Seq <= previous {
			t.Fatalf("step %d: seq %d after %d", step, envelope.Seq, previous)
		}
		previous = envelope.Seq
	}
}

// Test flow:
//  1. Accept a payload stamped 20 s ahead of this gateway's clock.
//  2. Move the clock past the maximum age, counted from the arrival.
//  3. Assert the snapshot has aged out.
func TestSnapshotStampedInTheFutureAgesFromItsArrival(t *testing.T) {
	clock := newFakeClock()
	view := NewView(loadMaxAge, fixedConcurrency(10), clock.now)
	topic := newLoadTopic(nil, DefaultRounding(), view)
	id := PeerID{Address: secondAddress, Instance: "other"}
	envelope := peerEnvelope{IssuedAtMS: clock.now().Add(20 * time.Second).UnixMilli(), Payload: encodeLoadPayload(hostRow(1))}
	if err := topic.Accept(id, envelope, clock.now()); err != nil {
		t.Fatal(err)
	}

	clock.advance(loadMaxAge + time.Millisecond)

	if got := view.freshPeers(); len(got) != 0 {
		t.Fatalf("freshPeers() = %v, want none", got)
	}
}

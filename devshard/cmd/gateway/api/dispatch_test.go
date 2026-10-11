package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"testing"

	"devshard/cmd/gateway/engine"
	"devshard/cmd/gateway/internal/leakcheck"
	"devshard/cmd/gateway/registry"
	"devshard/host"
	"devshard/internal/testutil"
	"devshard/signing"
	"devshard/state"
	"devshard/transport"
	"devshard/types"
	"devshard/user"
)

func TestMain(m *testing.M) {
	leakcheck.VerifyTestMain(m)
}

const (
	liveEscrowID = "7"
	liveModel    = "qwen"
)

var (
	errHostGone     = errors.New("host gone")
	errApplyFailure = errors.New("signature rejected")
	livePrompt      = []byte(`{"messages":[{"role":"user","content":"hello"}],"max_tokens":64}`)
)

func liveParams() user.InferenceParams {
	return user.InferenceParams{
		Model:       liveModel,
		Prompt:      livePrompt,
		InputLength: uint64(len(livePrompt)),
		MaxTokens:   64,
		StartedAt:   1_700_000_000,
	}
}

// newLiveSession builds a real *user.Session over in-memory storage, with hostClients as the only injected part.
func newLiveSession(t *testing.T, hostClients ...user.HostClient) (*user.Session, *state.StateMachine) {
	t.Helper()
	signers := []*signing.Secp256k1Signer{testutil.MustGenerateKey(t), testutil.MustGenerateKey(t)}
	group := testutil.MakeGroup(signers)
	config := testutil.DefaultConfig(len(group))
	creator := testutil.MustGenerateKey(t)
	verifier := signing.NewSecp256k1Verifier()
	store := testutil.MustMemoryStore(t, liveEscrowID, creator.Address(), config, group, 1_000_000)

	machine, err := state.NewStateMachine(liveEscrowID, config, group, 1_000_000, creator.Address(), verifier, store)
	if err != nil {
		t.Fatalf("NewStateMachine = %v, want nil", err)
	}
	clients := make([]user.HostClient, len(group))
	copy(clients, hostClients)
	session, err := user.NewSession(machine, creator, liveEscrowID, group, clients, verifier, user.WithStorage(store))
	if err != nil {
		t.Fatalf("NewSession = %v, want nil", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session, machine
}

// prepareNonce commits one real nonce through the session, for use as a genuine prepared inference.
func prepareNonce(t *testing.T, session *user.Session) *user.PreparedInference {
	t.Helper()
	prepared, err := session.PrepareInferenceFn(func(user.HostBinding) (user.InferenceParams, bool, context.Context, error) {
		return liveParams(), false, nil, nil
	})
	if err != nil {
		t.Fatalf("PrepareInferenceFn = %v, want nil", err)
	}
	return prepared
}

type appliedReply struct {
	hostIdx int
	nonce   uint64
	reply   *host.HostResponse
}

// recordingSession records the order of the calls a dispatch makes and what each was handed.
type recordingSession struct {
	calls    []string
	sent     *user.PreparedInference
	stream   io.Writer
	receipts int
	applied  appliedReply

	reply    *host.HostResponse
	sendErr  error
	applyErr error
}

func (s *recordingSession) SendOnly(_ context.Context, prepared *user.PreparedInference, stream io.Writer, onReceipt func()) (*host.HostResponse, error) {
	s.calls = append(s.calls, "SendOnly")
	s.sent, s.stream = prepared, stream
	if onReceipt != nil {
		onReceipt()
	}
	return s.reply, s.sendErr
}

func (s *recordingSession) ProcessResponse(hostIdx int, reply *host.HostResponse, inferenceNonce uint64) error {
	s.calls = append(s.calls, "ProcessResponse")
	s.applied = appliedReply{hostIdx: hostIdx, nonce: inferenceNonce, reply: reply}
	return s.applyErr
}

func (s *recordingSession) PinPendingFinish(uint64) { s.calls = append(s.calls, "PinPendingFinish") }
func (s *recordingSession) UnpinPendingFinish(uint64) {
	s.calls = append(s.calls, "UnpinPendingFinish")
}

func (s *recordingSession) IsNonceFinished(uint64) bool        { return true }
func (s *recordingSession) HostParticipantKeyList() []string   { return []string{"hostA", "hostB"} }
func (s *recordingSession) RewindHostCatchUp(int, string) bool { return true }

func (s *recordingSession) HostLabel(hostIdx int) string { return fmt.Sprintf("host-%d", hostIdx) }

type foreignNonce struct{}

func (foreignNonce) Nonce() uint64 { return 3 }
func (foreignNonce) HostIdx() int  { return 1 }

// Test flow:
//  1. Prepare a real nonce on a live session and wrap a `recordingSession` in an `escrowTarget`.
//  2. Call `target.Send` with a receipt callback.
//  3. Assert the calls happened in order: SendOnly, PinPendingFinish, ProcessResponse.
//  4. Assert SendOnly received the same prepared nonce and the caller's stream unwrapped, and the receipt callback fired once.
//  5. Assert ProcessResponse applied the reply with the prepared nonce's host index and nonce.
//  6. Assert the returned response reports Confirmed, since the reply carried a ConfirmedAt.
func TestSendDispatchesThenAppliesTheReply(t *testing.T) {
	t.Parallel()
	session, _ := newLiveSession(t)
	prepared := prepareNonce(t, session)
	recorded := &recordingSession{reply: &host.HostResponse{ConfirmedAt: 1_700_000_000, StreamBytesRead: 4096}}
	target := escrowTarget{session: recorded}

	response, err := target.Send(context.Background(), prepared, io.Discard, func() { recorded.receipts++ })
	if err != nil {
		t.Fatalf("Send = %v, want nil", err)
	}
	if got, want := recorded.calls, []string{"SendOnly", "PinPendingFinish", "ProcessResponse"}; !slices.Equal(got, want) {
		t.Errorf("call order = %v, want %v", got, want)
	}
	if recorded.sent != prepared {
		t.Errorf("SendOnly received %p, want the prepared nonce %p", recorded.sent, prepared)
	}
	if recorded.stream != io.Discard {
		t.Error("SendOnly received a wrapped stream, want the client writer unchanged")
	}
	if recorded.receipts != 1 {
		t.Errorf("receipt callbacks = %d, want 1 (the engine's own callback, passed through)", recorded.receipts)
	}
	want := appliedReply{hostIdx: prepared.HostIdx(), nonce: prepared.Nonce(), reply: recorded.reply}
	if recorded.applied != want {
		t.Errorf("ProcessResponse applied %+v, want %+v", recorded.applied, want)
	}
	if !response.Confirmed() {
		t.Error("Confirmed() = false, want true for a reply carrying a ConfirmedAt")
	}
}

// Test flow:
//  1. Send through a `recordingSession` and get back a response.
//  2. Call `response.ReleaseFinish` twice.
//  3. Assert UnpinPendingFinish appears exactly once in the recorded call order.
func TestAReplysFinishIsReleasedOnceHoweverOftenItIsAsked(t *testing.T) {
	t.Parallel()
	session, _ := newLiveSession(t)
	prepared := prepareNonce(t, session)
	recorded := &recordingSession{reply: &host.HostResponse{}}
	target := escrowTarget{session: recorded}
	response, err := target.Send(context.Background(), prepared, io.Discard, nil)
	if err != nil {
		t.Fatalf("Send = %v, want nil", err)
	}

	response.ReleaseFinish()
	response.ReleaseFinish()

	if got, want := recorded.calls, []string{"SendOnly", "PinPendingFinish", "ProcessResponse", "UnpinPendingFinish"}; !slices.Equal(got, want) {
		t.Errorf("call order = %v, want %v", got, want)
	}
}

// Test flow:
//  1. Define a table of send/apply outcomes, varying across a clean reply, a reply that arrived beside a send error, no reply at all, an unapplicable reply, and both a failed send and a failed apply together.
//  2. For each case, call `target.Send` against a `recordingSession` configured with that outcome.
//  3. Assert the returned error matches the case's expected error.
//  4. Assert the recorded call order matches the case's expectation.
//  5. Assert a response is returned only when the case expects one, so a typed nil is never handed to the engine.
func TestSendAppliesEveryReplyTheHostProduced(t *testing.T) {
	t.Parallel()
	reply := &host.HostResponse{StreamBytesRead: 12}
	cases := []struct {
		name         string
		reply        *host.HostResponse
		sendErr      error
		applyErr     error
		wantCalls    []string
		wantResponse bool
		wantErr      error
	}{
		{
			name:         "a clean reply",
			reply:        reply,
			wantCalls:    []string{"SendOnly", "PinPendingFinish", "ProcessResponse"},
			wantResponse: true,
		},
		{
			name:         "a reply that arrived beside an error is still applied",
			reply:        reply,
			sendErr:      errHostGone,
			wantCalls:    []string{"SendOnly", "PinPendingFinish", "ProcessResponse"},
			wantResponse: true,
			wantErr:      errHostGone,
		},
		{
			name:      "no reply leaves nothing to apply",
			sendErr:   errHostGone,
			wantCalls: []string{"SendOnly"},
			wantErr:   errHostGone,
		},
		{
			name:         "an unapplicable reply fails the attempt",
			reply:        reply,
			applyErr:     errApplyFailure,
			wantCalls:    []string{"SendOnly", "PinPendingFinish", "ProcessResponse"},
			wantResponse: true,
			wantErr:      errApplyFailure,
		},
		{
			name:         "a failed send and a failed apply both reach the caller",
			reply:        reply,
			sendErr:      errHostGone,
			applyErr:     errApplyFailure,
			wantCalls:    []string{"SendOnly", "PinPendingFinish", "ProcessResponse"},
			wantResponse: true,
			wantErr:      errHostGone,
		},
	}
	session, _ := newLiveSession(t)
	prepared := prepareNonce(t, session)

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			recorded := &recordingSession{reply: testCase.reply, sendErr: testCase.sendErr, applyErr: testCase.applyErr}
			target := escrowTarget{session: recorded}

			response, err := target.Send(context.Background(), prepared, io.Discard, nil)

			if !errors.Is(err, testCase.wantErr) {
				t.Errorf("Send error = %v, want it to match %v", err, testCase.wantErr)
			}
			if got := recorded.calls; !slices.Equal(got, testCase.wantCalls) {
				t.Errorf("call order = %v, want %v", got, testCase.wantCalls)
			}
			if got := response != nil; got != testCase.wantResponse {
				t.Errorf("response present = %t, want %t (a typed nil would make the engine read it)", got, testCase.wantResponse)
			}
		})
	}
}

// Test flow:
//  1. Define a table of send/apply errors, varying across a host that could not apply the diff, a host state hash differing from the local root, both a failed send and a state hash mismatch together, and an ordinary dispatch failure with neither.
//  2. For each case, call `target.Send`.
//  3. Assert whether the returned error matches `engine.ErrStateRootDivergence` matches the case's expectation.
func TestSendReportsADivergedStateRootFromEitherSideOfTheWire(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		sendErr       error
		applyErr      error
		wantDivergent bool
	}{
		{
			name: "the host could not apply the diff",
			sendErr: &transport.UpstreamStatusError{
				Path:       "/v1/chat/completions",
				StatusCode: http.StatusInternalServerError,
				Body:       "apply diff nonce 1: post_state_root does not match computed state root: abcd",
			},
			wantDivergent: true,
		},
		{
			name:          "the host's state hash differs from the local root",
			applyErr:      fmt.Errorf("%w: host 1 at nonce 1", types.ErrStateHashMismatch),
			wantDivergent: true,
		},
		{
			name:          "a state hash mismatch beside a failed send is still divergence",
			sendErr:       errHostGone,
			applyErr:      fmt.Errorf("%w: host 1 at nonce 1", types.ErrStateHashMismatch),
			wantDivergent: true,
		},
		{
			name:    "an ordinary dispatch failure is not divergence",
			sendErr: errHostGone,
		},
	}
	session, _ := newLiveSession(t)
	prepared := prepareNonce(t, session)

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			recorded := &recordingSession{
				reply:    &host.HostResponse{},
				sendErr:  testCase.sendErr,
				applyErr: testCase.applyErr,
			}
			target := escrowTarget{session: recorded}

			_, err := target.Send(context.Background(), prepared, io.Discard, nil)

			if got := errors.Is(err, engine.ErrStateRootDivergence); got != testCase.wantDivergent {
				t.Errorf("errors.Is(%v, ErrStateRootDivergence) = %t, want %t", err, got, testCase.wantDivergent)
			}
		})
	}
}

// Test flow:
//  1. Configure a `recordingSession` whose send fails with an `*transport.UpstreamStatusError`.
//  2. Call `target.Send`.
//  3. Assert `errors.As` recovers that exact upstream error, so the engine can still classify on it.
func TestSendKeepsTheUpstreamErrorInspectable(t *testing.T) {
	t.Parallel()
	session, _ := newLiveSession(t)
	prepared := prepareNonce(t, session)
	upstream := &transport.UpstreamStatusError{
		Path:       "/v1/chat/completions",
		StatusCode: http.StatusInternalServerError,
		Body:       "apply diff nonce 1: post_state_root does not match computed state root: abcd",
	}
	target := escrowTarget{session: &recordingSession{sendErr: upstream}}

	_, err := target.Send(context.Background(), prepared, io.Discard, nil)

	var found *transport.UpstreamStatusError
	if !errors.As(err, &found) || found != upstream {
		t.Errorf("errors.As(%v) did not recover the upstream status the engine classifies on", err)
	}
}

// Test flow:
//  1. Call `target.Send` with a `foreignNonce` the session never prepared.
//  2. Assert it returns an error naming the wrong nonce type and no response.
//  3. Assert the session recorded no calls.
func TestSendRefusesANonceItCannotDispatch(t *testing.T) {
	t.Parallel()
	recorded := &recordingSession{}
	target := escrowTarget{session: recorded}

	response, err := target.Send(context.Background(), foreignNonce{}, io.Discard, nil)

	if err == nil {
		t.Fatal("Send = nil error, want one naming the wrong nonce type")
	}
	if response != nil {
		t.Errorf("Send returned %v, want no response", response)
	}
	if len(recorded.calls) != 0 {
		t.Errorf("calls = %v, want none: a nonce the session did not prepare must not reach it", recorded.calls)
	}
}

// clientSink stands in for the HTTP response writer, and satisfies http.Flusher.
type clientSink struct {
	written []byte
	flushes int
}

func (s *clientSink) Write(chunk []byte) (int, error) {
	s.written = append(s.written, chunk...)
	return len(chunk), nil
}

func (s *clientSink) Flush() { s.flushes++ }

// flushingHost mirrors transport.writeSSELine: it writes a line and flushes only if the writer satisfies http.Flusher.
type flushingHost struct {
	line       string
	sawFlusher bool
}

func (h *flushingHost) Send(_ context.Context, req host.HostRequest, stream io.Writer, onReceipt func(*host.HostResponse)) (*host.HostResponse, error) {
	if onReceipt != nil {
		onReceipt(&host.HostResponse{})
	}
	if _, err := io.WriteString(stream, h.line); err != nil {
		return nil, err
	}
	flusher, ok := stream.(http.Flusher)
	h.sawFlusher = ok
	if ok {
		flusher.Flush()
	}
	return &host.HostResponse{
		Nonce:           req.Nonce,
		StreamBytesRead: int64(len(h.line)),
		Mempool: []*types.DevshardTx{{Tx: &types.DevshardTx_FinishInference{
			FinishInference: &types.MsgFinishInference{InferenceId: req.Nonce, EscrowId: liveEscrowID},
		}}},
	}, nil
}

// Test flow:
//  1. Send a prepared nonce through a real session backed by two `flushingHost`s that finish the inference.
//  2. Assert `target.Send` succeeds.
//  3. Assert `target.NonceFinished` reports true afterward.
func TestSendSettlesTheNonceItDispatched(t *testing.T) {
	t.Parallel()
	upstream := &flushingHost{line: "data: [DONE]\n\n"}
	session, _ := newLiveSession(t, upstream, upstream)
	prepared := prepareNonce(t, session)
	target := escrowTarget{session: session}

	if _, err := target.Send(context.Background(), prepared, io.Discard, nil); err != nil {
		t.Fatalf("Send = %v, want nil", err)
	}

	if !target.NonceFinished(prepared.Nonce()) {
		t.Errorf("NonceFinished(%d) = false after a finished reply, want true: the nonce is stranded and its"+
			" vote will be posted against the wrong deadline", prepared.Nonce())
	}
}

// Test flow:
//  1. Send a prepared nonce through a real session backed by `flushingHost`s, writing into a `clientSink`.
//  2. Assert the host saw a writer implementing `http.Flusher`.
//  3. Assert the sink was flushed once and received exactly the host's line.
func TestSendLeavesTheClientWriterFlushable(t *testing.T) {
	t.Parallel()
	upstream := &flushingHost{line: "data: {\"choices\":[]}\n\n"}
	session, _ := newLiveSession(t, upstream, upstream)
	prepared := prepareNonce(t, session)
	target := escrowTarget{session: session}
	sink := &clientSink{}

	_, err := target.Send(context.Background(), prepared, sink, nil)
	if err != nil {
		t.Fatalf("Send = %v, want nil", err)
	}
	if !upstream.sawFlusher {
		t.Error("the writer reaching the transport is not an http.Flusher: a winner's bytes would sit in the server's buffer")
	}
	if got, want := sink.flushes, 1; got != want {
		t.Errorf("flushes reaching the client = %d, want %d", got, want)
	}
	if got, want := string(sink.written), upstream.line; got != want {
		t.Errorf("bytes reaching the client = %q, want %q", got, want)
	}
}

// Test flow:
//  1. Resolve a live escrow's target and hold it.
//  2. Assert the target reports the session's host count and host labels, and exactly one hold was taken.
//  3. Release the target twice.
//  4. Assert exactly one release reached the escrow registry despite the doubled call.
//  5. Assert resolving an unregistered escrow reports not found.
func TestTargetResolvesOnlyTheLiveEscrowAndHoldsItUntilReleased(t *testing.T) {
	t.Parallel()
	session, machine := newLiveSession(t)
	escrows := &fixedEscrows{escrowID: liveEscrowID, session: registry.NewSessionHandle(session, machine)}
	sessions := NewSessions(escrows)

	target, release, ok := sessions.Target(liveEscrowID)

	if !ok {
		t.Fatalf("Target(%q) = not found, want the live escrow", liveEscrowID)
	}
	if got, want := target.HostCount(), 2; got != want {
		t.Errorf("HostCount() = %d, want %d", got, want)
	}
	if got, want := target.HostLabel(1), session.HostLabel(1); got != want {
		t.Errorf("HostLabel(1) = %q, want %q", got, want)
	}
	if got := escrows.holds.Load(); got != 1 {
		t.Errorf("escrow holds taken = %d, want 1: the resolved target counts nothing in flight", got)
	}

	release()
	release()

	if got := escrows.releases.Load(); got != 1 {
		t.Errorf("escrow holds given back after a doubled release = %d, want 1", got)
	}
	if _, _, ok := sessions.Target("8"); ok {
		t.Error("Target(8) resolved an escrow that was never added, want not found")
	}
}

// Test flow:
//  1. Prepare a nonce on a live session without ever sending it.
//  2. Assert `target.NonceFinished` reports false before any reply was applied.
func TestTargetReportsWhetherTheNonceIsFinished(t *testing.T) {
	t.Parallel()
	session, _ := newLiveSession(t)
	prepared := prepareNonce(t, session)
	target := escrowTarget{session: session}

	if target.NonceFinished(prepared.Nonce()) {
		t.Error("NonceFinished = true before any reply was applied, want false")
	}
}

// Test flow:
//  1. Wire a `Sessions` adapter into `engine.Deps` as both its Targets and Timeouts.
//  2. Assert both fields are non-nil, proving the adapter satisfies the engine's unexported interfaces at compile time.
func TestSessionsSatisfyTheEngineDeps(t *testing.T) {
	t.Parallel()
	sessions := NewSessions(&fixedEscrows{})

	deps := engine.Deps{Targets: sessions, Timeouts: sessions.Poster}

	if deps.Targets == nil || deps.Timeouts == nil {
		t.Error("engine.Deps did not take the adapters, want both wired")
	}
}

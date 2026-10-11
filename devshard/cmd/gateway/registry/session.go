package registry

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"common/completionapi"

	"devshard/cmd/gateway/scheduler"
	"devshard/heightsync"
	"devshard/state"
	"devshard/types"
	"devshard/user"
)

const ghostMaxTokens = uint64(completionapi.MinTokensFloor)

var (
	// ghostPrompt is composed into the diff, never sent to a host. See routing.md, "Ghost burns".
	ghostPrompt = fmt.Appendf(nil, `{"messages":[{"role":"user","content":"."}],"max_tokens":%d}`, ghostMaxTokens)

	// errNonceDeclined leaves the bound nonce unconsumed, so the next caller sees the same nonce.
	errNonceDeclined = errors.New("nonce declined")
)

// HostDial is where one of an escrow's hosts answers. See README.md.
type HostDial struct {
	ParticipantKey string
	BaseURL        string
	RoutePrefix    string
}

// EscrowSession is one escrow's session as the registry uses it; two of its methods carry a trap. See README.md, "The two session kinds".
type EscrowSession interface {
	ParticipantKeys() []string
	HostDials() []HostDial
	HeightSyncView() heightsync.OperatorView
	WaitRouterCatalog(ctx context.Context) error
	WaitHeightSeedReady(ctx context.Context) error
	HostParticipantKeyList() []string
	Nonce() uint64
	Balance() uint64
	TokenPrice() uint64
	FeePerNonce() uint64
	Phase() types.SessionPhase
	PrepareInferenceFn(chooser user.ParamsForHost) (*user.PreparedInference, error)
	Signatures() map[uint64]map[uint32][]byte
	SignedSlots() map[uint64]types.Bitmap128
	SignatureStatus() (entries []user.SignatureStatusEntry, highestQuorum uint64, hasAny bool)
	SnapshotState() types.EscrowState
	Inference(id uint64) (types.InferenceRecord, bool)
	ProtocolVersion() string
	LiveInferences() (types.SessionConfig, []types.InferenceRecord)
	SealedInferences() int
	PendingTxs() []*types.DevshardTx
	SendPendingDiff(ctx context.Context) error
	Finalize(ctx context.Context) error
	FlushSnapshot() error
	Close() error
	UserSession() *user.Session
	SweepExecutionTimeouts(ctx context.Context, grace time.Duration, budget int) user.SweepReport
}

// SessionFactory: an escrow with no record must fail wrapping escrow.ErrUnknownEscrow. See README.md, "The two session kinds".
type SessionFactory func(ctx context.Context, escrowID string) (EscrowSession, error)

type sessionHandle struct {
	*user.Session
	machine         *state.StateMachine
	finalizing      *sync.Mutex
	protocolVersion string
}

// NewSessionHandle takes the protocol version once. See README.md, "The two session kinds".
func NewSessionHandle(session *user.Session, machine *state.StateMachine) EscrowSession {
	return sessionHandle{
		Session:         session,
		machine:         machine,
		finalizing:      &sync.Mutex{},
		protocolVersion: machine.SnapshotStateNoInferences().StateRootAndProtocolVersion,
	}
}

func (h sessionHandle) Finalize(ctx context.Context) error {
	h.finalizing.Lock()
	defer h.finalizing.Unlock()
	for h.machine.Phase() == types.PhaseFinalizing {
		nonce := h.Nonce()
		if err := h.SendPendingDiff(ctx); err != nil && h.Nonce() == nonce {
			return fmt.Errorf("resuming the finalize cut short at nonce %d: %w", nonce, err)
		}
	}
	return h.Session.Finalize(ctx)
}

// SweepExecutionTimeouts steps over an escrow a Finalize holds instead of waiting it out, and votes only while the session is active.
func (h sessionHandle) SweepExecutionTimeouts(ctx context.Context, grace time.Duration, budget int) user.SweepReport {
	if !h.finalizing.TryLock() {
		return user.SweepReport{}
	}
	defer h.finalizing.Unlock()
	if h.machine.Phase() != types.PhaseActive {
		return user.SweepReport{}
	}
	return h.Session.SweepExecutionTimeouts(ctx, grace, budget)
}

func (h sessionHandle) Phase() types.SessionPhase        { return h.machine.Phase() }
func (h sessionHandle) SnapshotState() types.EscrowState { return h.machine.SnapshotState() }
func (h sessionHandle) ProtocolVersion() string          { return h.protocolVersion }
func (h sessionHandle) SealedInferences() int            { return len(h.machine.ExportSealedNonces()) }
func (h sessionHandle) UserSession() *user.Session       { return h.Session }

func (h sessionHandle) LiveInferences() (types.SessionConfig, []types.InferenceRecord) {
	ids := h.machine.LiveInferenceIDs()
	records := make([]types.InferenceRecord, 0, len(ids))
	for id := range ids {
		if record, tracked := h.machine.GetInference(id); tracked {
			records = append(records, record)
		}
	}
	return h.machine.Config(), records
}

func (h sessionHandle) SessionConfig() types.SessionConfig    { return h.machine.Config() }
func (h sessionHandle) AppliedNonce() uint64                  { return h.machine.LatestNonce() }
func (h sessionHandle) LiveInferenceIDs() map[uint64]struct{} { return h.machine.LiveInferenceIDs() }

func (h sessionHandle) Inference(id uint64) (types.InferenceRecord, bool) {
	return h.machine.GetInference(id)
}

type hostDialer interface {
	BaseURL() string
	RoutePrefix() string
}

// HeightSyncView serves the cache only: taking a fresh snapshot on a debug read would move the
// producer's own state. An unwired session reports nothing rather than an empty shape.
func (h sessionHandle) HeightSyncView() heightsync.OperatorView {
	if !h.Session.HeightSyncWired() {
		return heightsync.OperatorView{}
	}
	return h.Session.CachedHeightSyncView()
}

func (h sessionHandle) HostDials() []HostDial {
	clients := h.Session.Clients()
	keys := h.Session.HostParticipantKeyList()
	dials := make([]HostDial, 0, len(clients))
	for slot, client := range clients {
		dialer, addressable := client.(hostDialer)
		if !addressable || dialer == nil {
			continue
		}
		key := ""
		if slot < len(keys) {
			key = keys[slot]
		}
		dials = append(dials, HostDial{ParticipantKey: key, BaseURL: dialer.BaseURL(), RoutePrefix: dialer.RoutePrefix()})
	}
	return dials
}

// slots is taken once: the group is fixed for the life of a session, and asking the session takes the lock a nonce commit holds.
type nonceStream struct {
	session EscrowSession
	model   string
	slots   []string
	now     func() time.Time
}

// newNonceStream is the only way to build one, so the cached group cannot disagree with the session's.
func newNonceStream(session EscrowSession, model string, now func() time.Time) nonceStream {
	return nonceStream{session: session, model: model, slots: session.HostParticipantKeyList(), now: now}
}

func (s nonceStream) ParticipantKeys() []string  { return s.session.ParticipantKeys() }
func (s nonceStream) SlotParticipants() []string { return s.slots }
func (s nonceStream) GroupSize() int             { return len(s.slots) }
func (s nonceStream) LatestNonce() uint64        { return s.session.Nonce() }
func (s nonceStream) Balance() uint64            { return s.session.Balance() }
func (s nonceStream) TokenPrice() uint64         { return s.session.TokenPrice() }
func (s nonceStream) FeePerNonce() uint64        { return s.session.FeePerNonce() }

func (s nonceStream) Advance(decide func(scheduler.HostBinding) scheduler.NonceIntent) (scheduler.Prepared, error) {
	prepared, err := s.session.PrepareInferenceFn(func(binding user.HostBinding) (user.InferenceParams, bool, context.Context, error) {
		intent := decide(scheduler.HostBinding{
			Nonce:       binding.Nonce,
			HostIdx:     binding.HostIdx,
			Participant: binding.ParticipantKey,
		})
		switch {
		case !intent.Commit:
			return user.InferenceParams{}, false, nil, errNonceDeclined
		case intent.Ghost:
			return s.ghostParams(), false, nil, nil
		}
		params, isInference := intent.Params.(user.InferenceParams)
		if !isInference {
			return user.InferenceParams{}, false, nil, fmt.Errorf("dispatch params are %T, want user.InferenceParams", intent.Params)
		}
		return params, false, nil, nil
	})
	switch {
	case errors.Is(err, errNonceDeclined):
		return nil, nil
	case err != nil:
		return nil, err
	case prepared == nil:
		return nil, nil
	}
	return prepared, nil
}

// StartedAt is seconds, not milliseconds, or the refusal deadline goes negative. See host/timeout.go, VerifyRefusedTimeout.
func (s nonceStream) ghostParams() user.InferenceParams {
	return user.InferenceParams{
		Model:       s.model,
		Prompt:      ghostPrompt,
		InputLength: uint64(len(ghostPrompt)),
		MaxTokens:   ghostMaxTokens,
		StartedAt:   s.now().Unix(),
	}
}

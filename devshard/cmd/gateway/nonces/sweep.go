package nonces

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"sync/atomic"
	"time"

	"devshard/cmd/gateway/accounting"
	"devshard/cmd/gateway/internal/logkey"
	"devshard/cmd/gateway/registry"
	"devshard/logging"
	"devshard/types"
)

// watchDiffs hands every composed diff to the journal, so the book's lock is never taken under the session's. See README.md, "Boundaries".
func (n *Recorder) watchDiffs(escrowID string, session registry.EscrowSession, diffs DiffJournal) {
	if diffs == nil {
		return
	}
	underlying := session.UserSession()
	if underlying == nil {
		return
	}
	if watched, already := n.observing.Swap(escrowID, underlying); already && watched == underlying {
		return
	}
	n.watchSessionDiffs(escrowID, underlying, diffs)
}

type diffSource interface {
	SetDiffObserver(observer func(types.Diff))
	Diffs() []types.Diff
}

type firstObservedDiff struct {
	seen  atomic.Bool
	nonce atomic.Uint64
}

func (first *firstObservedDiff) observe(nonce uint64) {
	if !first.seen.Load() {
		first.nonce.Store(nonce)
		first.seen.Store(true)
	}
}

func (n *Recorder) watchSessionDiffs(escrowID string, source diffSource, diffs DiffJournal) {
	first := &firstObservedDiff{}
	source.SetDiffObserver(func(diff types.Diff) {
		first.observe(diff.Nonce)
		diffs.DiffComposed(escrowID, &diff)
	})
	if n.service == nil {
		return
	}
	watermark, restored := n.service.Book.TakeRestoredWatermark(escrowID)
	if !restored {
		return
	}
	// A shutdown that abandoned work closes the journal while that work still composes diffs, so the last run's ledger never took them.
	// Every service diff it dropped lies above the watermark it saved; the replay hands over those still retained, up to the first the observer saw.
	retained := source.Diffs()
	seen, firstObserved := first.seen.Load(), first.nonce.Load()
	for index := range retained {
		diff := retained[index]
		if diff.Nonce > watermark && (!seen || diff.Nonce < firstObserved) && !startsInference(diff) {
			diffs.DiffComposed(escrowID, &diff)
		}
	}
}

func startsInference(diff types.Diff) bool {
	return slices.ContainsFunc(diff.Txs, func(tx *types.DevshardTx) bool { return tx.GetStartInference() != nil })
}

// Start serves the ledger's listener and sweeps until ctx ends; every escrow the sweep watches hands its composed diffs to diffs.
func (n *Recorder) Start(ctx context.Context, escrows EscrowSource, diffs DiffJournal) {
	if n == nil {
		return
	}
	n.serve()
	n.StartSweeping(ctx, escrows, diffs)
}

// StartSweeping sweeps until ctx ends without serving the listener, for a process that must not open a port.
func (n *Recorder) StartSweeping(ctx context.Context, escrows EscrowSource, diffs DiffJournal) {
	if n == nil {
		return
	}
	go n.sweepUntil(ctx, escrows, diffs)
}

func (n *Recorder) serve() {
	if n.listener == nil {
		return
	}
	logging.Info("nonce accounting listening", logkey.Addr, n.listener.Addr)
	go func() {
		if err := n.listener.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logging.Error("nonce accounting listener stopped", "error", err)
		}
	}()
}

func (n *Recorder) sweepUntil(ctx context.Context, escrows EscrowSource, diffs DiffJournal) {
	ticker := time.NewTicker(nonceAccountingSweepInterval)
	defer ticker.Stop()
	for {
		n.sweep(ctx, escrows, diffs)
		select {
		case <-ticker.C:
		case <-n.wake:
		case <-ctx.Done():
			return
		}
	}
}

func (n *Recorder) sweep(ctx context.Context, escrows EscrowSource, diffs DiffJournal) {
	states := escrows.Snapshot()
	published := make(map[string]struct{}, len(states))
	for _, state := range states {
		session, routable := escrows.RoutableSession(state.ID)
		if !routable {
			continue
		}
		published[state.ID] = struct{}{}
		escrowState := session.SnapshotState()
		if epoch, known := n.epochOf(ctx, state.ID); known {
			n.report(n.service.Book.OpenEscrow(accounting.EscrowMetadata{
				EscrowID:      state.ID,
				Model:         state.Model,
				CreationEpoch: epoch,
				Slots:         escrowState.Group,
			}))
		}
		n.observeEscrowState(state.ID, escrowState)
		n.closeSealed(state.ID, escrowState, session)
		n.watchDiffs(state.ID, session, diffs)
	}
	for _, escrowID := range n.service.Book.EscrowIDs() {
		if _, still := published[escrowID]; !still {
			n.service.Book.RetireEscrow(escrowID)
			n.observing.Delete(escrowID)
		}
	}
}

// What one sweep reads off an escrow: the watermark, what the chain charges per nonce, the chain's per-slot stats, and every nonce's money.
func (n *Recorder) observeEscrowState(escrowID string, escrowState types.EscrowState) {
	n.report(n.service.Book.ObserveLatestNonce(escrowID, escrowState.LatestNonce))
	n.report(n.service.Book.ObserveCharging(escrowID, escrowState.Config.FeePerNonce, escrowState.FinalizeNonce))
	for slotID, stats := range escrowState.HostStats {
		if stats != nil {
			n.report(n.service.Book.ObserveHostStats(escrowID, slotID, *stats))
		}
	}
	n.report(n.service.Book.ObserveInferences(escrowID, escrowState.Inferences))
}

// EscrowRetiring takes the reading the sweep will never take again, and asks the ledger's own goroutine to write it out.
// See docs/accounting.md, "Money and tokens".
func (n *Recorder) EscrowRetiring(escrowID string, session registry.EscrowSession) {
	if n == nil || n.service == nil {
		return
	}
	if session != nil {
		snapshot := session.SnapshotState()
		n.observeEscrowState(escrowID, snapshot)
		n.closeSealed(escrowID, snapshot, session)
	}
	n.service.Book.RetireEscrow(escrowID)
	n.observing.Delete(escrowID)
	n.service.RequestFlush()
}

func (n *Recorder) closeSealed(escrowID string, snapshot types.EscrowState, session registry.EscrowSession) {
	var finished []uint64
	for _, nonce := range n.service.Book.UnfinishedNonces(escrowID) {
		if _, live := snapshot.Inferences[nonce]; live || nonce > snapshot.LatestNonce {
			continue
		}
		if sealedFinished(session, nonce) {
			finished = append(finished, nonce)
		}
	}
	n.report(n.service.Book.MarkFinished(escrowID, finished))
}

func sealedFinished(session registry.EscrowSession, nonce uint64) bool {
	if record, kept := session.SealedInference(nonce); kept {
		return accounting.FinishApplied(record.Status)
	}
	underlying := session.UserSession()
	return underlying != nil && underlying.IsNonceFinished(nonce)
}

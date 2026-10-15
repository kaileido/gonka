package user

import (
	"context"

	"devshard/logging"
)

const laggingHostSyncNonces = maxRetainedDiffs

func (s *Session) syncLaggingHostsLocked() {
	if s.laggingSyncClosed {
		return
	}
	if s.laggingSyncAt == nil {
		s.laggingSyncAt = make(map[int]uint64)
	}
	for hostIdx := range s.clients {
		if s.nonce-s.hostSyncNonce[hostIdx] < laggingHostSyncNonces || s.nonce-s.laggingSyncAt[hostIdx] < laggingHostSyncNonces {
			continue
		}
		s.laggingSyncAt[hostIdx] = s.nonce
		if s.laggingSyncStop == nil {
			s.laggingSyncStop = make(chan struct{})
		}
		s.laggingSyncs.Add(1)
		go s.syncLaggingHost(s.laggingSyncStop, hostIdx)
	}
}

func (s *Session) syncLaggingHost(stop <-chan struct{}, hostIdx int) {
	defer s.laggingSyncs.Done()
	gate := s.hostCatchUpGate(hostIdx)
	if !gate.TryLock() {
		return
	}
	defer gate.Unlock()
	if gate.refusal(s.clock()) != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	s.mu.Lock()
	throughNonce := s.nonce
	s.mu.Unlock()
	err := s.sendCatchUpThrough(ctx, hostIdx, s.getFinalizeClients()[hostIdx], throughNonce)
	gate.record(ctx, err, s.clock())
	if err != nil {
		logging.Debug("lagging host sync did not reach the host", "subsystem", "session",
			"escrow", s.escrowID, "host", hostIdx, "through", throughNonce, "error", err)
	}
}

func (s *Session) stopLaggingSync() {
	s.mu.Lock()
	s.laggingSyncClosed = true
	if s.laggingSyncStop != nil {
		close(s.laggingSyncStop)
	}
	s.mu.Unlock()
	s.laggingSyncs.Wait()
}

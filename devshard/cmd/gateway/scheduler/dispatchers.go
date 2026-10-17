package scheduler

// Stop shuts every escrow's actor down and is safe to call more than once.
func (s *Scheduler) Stop() {
	s.registryMu.Lock()
	s.stopped = true
	running := make([]*dispatcher, 0, len(s.dispatchers))
	for _, active := range s.dispatchers {
		running = append(running, active)
	}
	clear(s.dispatchers)
	s.registryMu.Unlock()

	for _, active := range running {
		active.stop()
	}
}

func (s *Scheduler) dispatcherFor(escrow Escrow) (*dispatcher, error) {
	s.registryMu.Lock()
	defer s.registryMu.Unlock()
	if s.stopped {
		return nil, ErrDispatcherStopped
	}

	target, known := s.dispatchers[escrow.ID]
	if !known || target.isStopped() || target.sessionID != escrow.SessionID {
		target = newDispatcher(dispatcherDeps{
			escrowID:            escrow.ID,
			sessionID:           escrow.SessionID,
			session:             escrow.Session,
			snapshots:           s.snapshots,
			predicates:          s.predicates(escrow),
			acquireSlot:         s.acquireSlot(escrow),
			holdEscrow:          escrow.Hold,
			observer:            s.observer,
			now:                 s.now,
			matchWait:           s.matchWait(),
			maxConsecutiveBurns: s.maxConsecutiveBurns,
			retirementReserve:   s.retirementReserveFor(escrow),
			newTimer:            s.newTimer,
			retire:              s.retire,
			idleGrace:           idleDispatcherGrace,
			submitBuffer:        s.dispatchQueueDepth(),
			onExhausted:         s.onEscrowExhausted,
		})
		s.dispatchers[escrow.ID] = target
		target.start()
	}
	target.pendingSubmits.Add(1)
	return target, nil
}

func (s *Scheduler) retire(idle *dispatcher) bool {
	s.registryMu.Lock()
	defer s.registryMu.Unlock()
	if idle.pendingSubmits.Load() != 0 || !idle.markStopped() {
		return false
	}
	if s.dispatchers[idle.escrowID] == idle {
		delete(s.dispatchers, idle.escrowID)
	}
	if s.observer != nil {
		s.observer.EscrowRetired(idle.escrowID)
	}
	return true
}

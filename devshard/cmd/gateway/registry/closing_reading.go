package registry

type closingObserver func(escrowID string, session EscrowSession)

func (r *Registry) observeClosing(entry *escrowEntry) {
	if r.closingObserver != nil {
		r.closingObserver(entry.id, entry.session)
	}
}

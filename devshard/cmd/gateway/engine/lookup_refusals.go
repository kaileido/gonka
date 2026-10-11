package engine

import "sync"

// lookupRefusalsTrusted is how many escrow-lookup refusals in a row a host is believed; the header is the host's own word.
const lookupRefusalsTrusted = 3

// lookupRefusals excuses a host's escrow_lookup_limited only while it is not all the host ever answers. See ../docs/race.md, "The exemption ladder".
type lookupRefusals struct {
	mu          sync.Mutex
	consecutive map[hostModelKey]int
}

func newLookupRefusals() *lookupRefusals {
	return &lookupRefusals{consecutive: map[hostModelKey]int{}}
}

func (r *lookupRefusals) judge(attempt AttemptOutcome, model string) AttemptOutcome {
	if attempt.Terminal == TerminalUnclassified {
		return attempt
	}
	key := hostModelKey{participant: attempt.Participant, model: model}
	r.mu.Lock()
	defer r.mu.Unlock()
	if attempt.Terminal != TerminalEscrowLookupLimited {
		delete(r.consecutive, key)
		return attempt
	}
	r.consecutive[key]++
	if r.consecutive[key] > lookupRefusalsTrusted {
		attempt.Terminal = TerminalThrottled
	}
	return attempt
}

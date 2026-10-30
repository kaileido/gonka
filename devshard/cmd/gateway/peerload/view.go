package peerload

import (
	"cmp"
	"slices"
	"strings"
	"sync"
	"time"
)

// DiscountRow is the discount of one {participant, model} as /metrics reports it.
type DiscountRow struct {
	Participant string
	Model       string
	Foreign     float64
	Discount    float64
}

type hostKey struct {
	participant string
	model       string
}

type peerSnapshot struct {
	rows       map[hostKey]HostLoad
	freshAtMS  int64
	receivedAt time.Time
}

// View holds the latest snapshot of each peer instance and computes the shadow discount; only /metrics reads it.
type View struct {
	maxAge      time.Duration
	concurrency func(participant, model string) int64
	now         func() time.Time

	mu    sync.RWMutex
	peers map[PeerID]peerSnapshot
}

// NewView reads the receiver's own earned concurrency through concurrency, per participant and model.
func NewView(maxAge time.Duration, concurrency func(participant, model string) int64, now func() time.Time) *View {
	return &View{maxAge: maxAge, concurrency: concurrency, now: now, peers: map[PeerID]peerSnapshot{}}
}

// Store replaces one peer's snapshot; freshAtMS is the moment it ages out from.
func (v *View) Store(from PeerID, rows []HostLoad, freshAtMS int64) {
	indexed := make(map[hostKey]HostLoad, len(rows))
	for _, row := range rows {
		indexed[hostKey{participant: row.Participant, model: row.Model}] = row
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.peers[from] = peerSnapshot{rows: indexed, freshAtMS: freshAtMS, receivedAt: v.now()}
}

func (v *View) forget(id PeerID) {
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.peers, id)
}

func (v *View) freshPeers() []PeerID {
	v.mu.RLock()
	defer v.mu.RUnlock()
	fresh := make([]PeerID, 0, len(v.peers))
	for id, snapshot := range v.peers {
		if v.freshLocked(snapshot) {
			fresh = append(fresh, id)
		}
	}
	slices.SortFunc(fresh, func(a, b PeerID) int { return strings.Compare(a.String(), b.String()) })
	return fresh
}

// LastSeenSeconds returns how long ago each peer's latest accepted snapshot arrived, stale ones included.
func (v *View) LastSeenSeconds() map[PeerID]float64 {
	v.mu.RLock()
	defer v.mu.RUnlock()
	seen := make(map[PeerID]float64, len(v.peers))
	for id, snapshot := range v.peers {
		seen[id] = v.now().Sub(snapshot.receivedAt).Seconds()
	}
	return seen
}

func (v *View) discount(participant, model string) float64 {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return discountFor(v.foreignLocked(hostKey{participant: participant, model: model}), v.concurrency(participant, model))
}

// Rows reports each pair a fresh peer names and the receiver knows the concurrency of, so a peer cannot grow the label set past the receiver's own hosts.
func (v *View) Rows() []DiscountRow {
	v.mu.RLock()
	defer v.mu.RUnlock()
	seen := make(map[hostKey]struct{})
	for _, snapshot := range v.peers {
		if !v.freshLocked(snapshot) {
			continue
		}
		for key := range snapshot.rows {
			seen[key] = struct{}{}
		}
	}
	rows := make([]DiscountRow, 0, len(seen))
	for key := range seen {
		concurrency := v.concurrency(key.participant, key.model)
		if concurrency <= concurrencyUnknown {
			continue
		}
		foreign := v.foreignLocked(key)
		rows = append(rows, DiscountRow{Participant: key.participant, Model: key.model, Foreign: foreign, Discount: discountFor(foreign, concurrency)})
	}
	slices.SortFunc(rows, func(a, b DiscountRow) int {
		return cmp.Or(strings.Compare(a.Participant, b.Participant), strings.Compare(a.Model, b.Model))
	})
	return rows
}

func (v *View) foreignLocked(key hostKey) float64 {
	byAddress := make(map[string]float64)
	for id, snapshot := range v.peers {
		if v.freshLocked(snapshot) {
			byAddress[id.Address] += float64(snapshot.rows[key].InflightRequests)
		}
	}
	concurrency := float64(v.concurrency(key.participant, key.model))
	var foreign float64
	for _, load := range byAddress {
		foreign += min(load, concurrency)
	}
	return foreign
}

func (v *View) freshLocked(snapshot peerSnapshot) bool {
	return v.now().Sub(time.UnixMilli(snapshot.freshAtMS)) <= v.maxAge
}

func discountFor(foreign float64, concurrency int64) float64 {
	if concurrency <= concurrencyUnknown {
		return discountNeutral
	}
	return max(discountFloor, min(discountNeutral, discountNeutral-foreign/float64(concurrency)))
}

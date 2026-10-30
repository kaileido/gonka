package peerload

import (
	"math"
	"slices"
	"testing"
	"time"
)

func fixedConcurrency(concurrency int64) func(string, string) int64 {
	return func(string, string) int64 { return concurrency }
}

func hostRow(inflight uint32) []HostLoad {
	return []HostLoad{{Participant: testHost, Model: testModel, InflightRequests: inflight}}
}

// Test flow:
//  1. Store load from no peer, a little, half, and past the floor.
//  2. Assert the discount is 1, 1 - foreign/conc, and never under the floor.
func TestDiscountFollowsForeignLoadDownToTheFloor(t *testing.T) {
	cases := []struct {
		inflight uint32
		want     float64
	}{{0, 1}, {2, 0.8}, {5, 0.5}, {8, 0.25}, {10, 0.25}}
	for _, testCase := range cases {
		clock := newFakeClock()
		view := NewView(loadMaxAge, fixedConcurrency(10), clock.now)
		view.Store(PeerID{Address: secondAddress, Instance: "other"}, hostRow(testCase.inflight), clock.now().UnixMilli())
		if got := view.discount(testHost, testModel); math.Abs(got-testCase.want) > 1e-9 {
			t.Errorf("Discount(inflight %d) = %v, want %v", testCase.inflight, got, testCase.want)
		}
	}
}

// Test flow:
//  1. Store load split over two replicas of one address, and the same sum from one instance.
//  2. Store load past conc from one address next to another address.
//  3. Assert splitting changes nothing, and each address is clamped to conc on its own.
func TestReplicasSumBeforeTheAddressIsClamped(t *testing.T) {
	clock := newFakeClock()
	fresh := clock.now().UnixMilli()
	split := NewView(loadMaxAge, fixedConcurrency(10), clock.now)
	split.Store(PeerID{Address: secondAddress, Instance: "one"}, hostRow(2), fresh)
	split.Store(PeerID{Address: secondAddress, Instance: "two"}, hostRow(2), fresh)
	whole := NewView(loadMaxAge, fixedConcurrency(10), clock.now)
	whole.Store(PeerID{Address: secondAddress, Instance: "one"}, hostRow(4), fresh)
	if split.discount(testHost, testModel) != whole.discount(testHost, testModel) {
		t.Fatalf("split %v, whole %v", split.discount(testHost, testModel), whole.discount(testHost, testModel))
	}

	clamped := NewView(loadMaxAge, fixedConcurrency(100), clock.now)
	clamped.Store(PeerID{Address: secondAddress, Instance: "one"}, hostRow(500), fresh)
	clamped.Store(PeerID{Address: secondAddress, Instance: "two"}, hostRow(500), fresh)
	rows := clamped.Rows()
	if len(rows) != 1 || rows[0].Foreign != 100 {
		t.Fatalf("Rows() = %+v, want foreign clamped to 100", rows)
	}
}

// Test flow:
//  1. Store a snapshot, advance to the last fresh moment, then past it.
//  2. Assert it counts, then does not, and the discount returns to 1.
func TestStaleSnapshotsAgeOut(t *testing.T) {
	clock := newFakeClock()
	view := NewView(loadMaxAge, fixedConcurrency(10), clock.now)
	id := PeerID{Address: secondAddress, Instance: "other"}
	view.Store(id, hostRow(5), clock.now().UnixMilli())

	clock.advance(loadMaxAge)
	if got := view.freshPeers(); !slices.Equal(got, []PeerID{id}) {
		t.Fatalf("FreshPeers() = %v at the limit", got)
	}

	clock.advance(time.Millisecond)
	if got := view.freshPeers(); len(got) != 0 {
		t.Fatalf("FreshPeers() = %v past the limit", got)
	}
	if got := view.discount(testHost, testModel); got != discountNeutral {
		t.Fatalf("Discount() = %v, want 1", got)
	}
	if got := view.Rows(); len(got) != 0 {
		t.Fatalf("Rows() = %+v past the limit", got)
	}
}

// Test flow:
//  1. Store a row for a pair whose concurrency the receiver does not know.
//  2. Assert the discount is 1 and no metric row is reported for it.
func TestUnknownConcurrencyIsNeutralAndAddsNoSeries(t *testing.T) {
	clock := newFakeClock()
	view := NewView(loadMaxAge, fixedConcurrency(concurrencyUnknown), clock.now)
	view.Store(PeerID{Address: secondAddress, Instance: "other"}, hostRow(50), clock.now().UnixMilli())

	if got := view.discount(testHost, testModel); got != discountNeutral {
		t.Fatalf("Discount() = %v, want 1", got)
	}
	if got := view.Rows(); len(got) != 0 {
		t.Fatalf("Rows() = %+v, want none", got)
	}
}

// Test flow:
//  1. Store a second snapshot for one peer.
//  2. Forget the peer.
//  3. Assert the view keeps one snapshot per peer and none after the forget.
func TestViewKeepsOneSnapshotPerPeer(t *testing.T) {
	clock := newFakeClock()
	view := NewView(loadMaxAge, fixedConcurrency(10), clock.now)
	id := PeerID{Address: secondAddress, Instance: "other"}
	view.Store(id, hostRow(5), clock.now().UnixMilli())
	view.Store(id, hostRow(2), clock.now().UnixMilli())

	if got := view.discount(testHost, testModel); math.Abs(got-0.8) > 1e-9 {
		t.Fatalf("Discount() = %v, want the latest snapshot's 0.8", got)
	}
	view.forget(id)
	if got := len(view.LastSeenSeconds()); got != 0 {
		t.Fatalf("view holds %d peers after Forget", got)
	}
}

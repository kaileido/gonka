package metrics

import (
	"testing"
	"time"

	"devshard/cmd/gateway/peerload"
)

type fakePeerLoadSource struct {
	view *peerload.View
}

func (f fakePeerLoadSource) View() *peerload.View { return f.view }

func (f fakePeerLoadSource) HandshakeCounts() map[string]uint64 {
	return map[string]uint64{peerload.HandshakeFingerprint: 2}
}

// Test flow:
//  1. Register the collector over a view holding one fresh peer's load.
//  2. Gather.
//  3. Assert the discount, the foreign load, the last-seen age and the handshake count are exported.
func TestPeerLoadCollectorExportsTheShadowDiscount(t *testing.T) {
	now := time.UnixMilli(1_800_000_000_000)
	view := peerload.NewView(30*time.Second, func(string, string) int64 { return 10 }, func() time.Time { return now })
	view.Store(peerload.PeerID{Address: "gonka1a", Instance: "one"}, []peerload.HostLoad{{Participant: "gonka1host", Model: "model", InflightRequests: 5}}, now.UnixMilli())
	telemetry := New()
	telemetry.Register(NewPeerLoadCollector(fakePeerLoadSource{view: view}))

	hostLabels := labels{"participant": "gonka1host", "model": "model"}
	discount := matching(family(t, telemetry, "devshard_gateway_peer_discount").GetMetric(), hostLabels)
	if discount == nil || discount.GetGauge().GetValue() != 0.5 {
		t.Fatalf("discount = %v, want 0.5", discount)
	}
	foreign := matching(family(t, telemetry, "devshard_gateway_peer_foreign_inflight").GetMetric(), hostLabels)
	if foreign == nil || foreign.GetGauge().GetValue() != 5 {
		t.Fatalf("foreign = %v, want 5", foreign)
	}
	lastSeen := matching(family(t, telemetry, "devshard_gateway_peer_last_seen_seconds").GetMetric(), labels{"peer": "gonka1a", "peer_instance": "one"})
	if lastSeen == nil || lastSeen.GetGauge().GetValue() != 0 {
		t.Fatalf("last seen = %v, want 0", lastSeen)
	}
	handshakes := matching(family(t, telemetry, "devshard_gateway_peer_handshakes_total").GetMetric(), labels{"result": peerload.HandshakeFingerprint})
	if handshakes == nil || handshakes.GetCounter().GetValue() != 2 {
		t.Fatalf("handshakes = %v, want 2", handshakes)
	}
}

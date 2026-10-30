package metrics

import (
	"github.com/prometheus/client_golang/prometheus"

	"devshard/cmd/gateway/peerload"
)

// PeerLoadSource is the channel's state the collector reads at each scrape.
type PeerLoadSource interface {
	View() *peerload.View
	HandshakeCounts() map[string]uint64
}

// PeerLoadCollector exports the peer channel; it is registered only when the channel is active.
type PeerLoadCollector struct {
	source     PeerLoadSource
	lastSeen   *prometheus.Desc
	handshakes *prometheus.Desc
	foreign    *prometheus.Desc
	discount   *prometheus.Desc
}

func NewPeerLoadCollector(source PeerLoadSource) *PeerLoadCollector {
	return &PeerLoadCollector{
		source:     source,
		lastSeen:   gaugeDesc("devshard_gateway_peer_last_seen_seconds", "Seconds since this peer instance's last accepted snapshot.", "peer", "peer_instance"),
		handshakes: counterDesc("devshard_gateway_peer_handshakes_total", "Peer channel handshakes, by the verifier's result.", "result"),
		foreign:    gaugeDesc("devshard_gateway_peer_foreign_inflight", "Requests other gateways report in flight on this host, each address clamped to this gateway's own concurrency for it.", "participant", "model"),
		discount:   gaugeDesc("devshard_gateway_peer_discount", "The escrow-score discount computed from peer load. Shadow value: no limit or score reads it.", "participant", "model"),
	}
}

func (c *PeerLoadCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.lastSeen
	ch <- c.handshakes
	ch <- c.foreign
	ch <- c.discount
}

func (c *PeerLoadCollector) Collect(ch chan<- prometheus.Metric) {
	view := c.source.View()
	for peer, seconds := range view.LastSeenSeconds() {
		gauge(ch, c.lastSeen, seconds, peer.Address, peer.Instance)
	}
	for result, count := range c.source.HandshakeCounts() {
		counter(ch, c.handshakes, float64(count), result)
	}
	for _, row := range view.Rows() {
		gauge(ch, c.foreign, row.Foreign, row.Participant, row.Model)
		gauge(ch, c.discount, row.Discount, row.Participant, row.Model)
	}
}

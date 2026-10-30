package peerload

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"slices"
	"testing"
	"time"
)

const redialRounds = 50

type loopbackCluster struct {
	registry *fakeRegistry
	clock    *fakeClock
	replicaA *loopbackInstance
	replicaB *loopbackInstance
	other    *loopbackInstance
	all      []*loopbackInstance
}

func newLoopbackCluster(t *testing.T, samples func() []LoadSample, concurrency func(string, string) int64) *loopbackCluster {
	t.Helper()
	registry := newFakeRegistry()
	clock := newFakeClock()
	replicaA := newLoopbackInstance(t, PeerID{Address: firstAddress, Instance: "replica-a"}, registry, clock, samples, concurrency)
	replicaB := newLoopbackInstance(t, PeerID{Address: firstAddress, Instance: "replica-b"}, registry, clock, samples, concurrency)
	other := newLoopbackInstance(t, PeerID{Address: secondAddress, Instance: "other"}, registry, clock, samples, concurrency)
	cluster := &loopbackCluster{registry: registry, clock: clock, replicaA: replicaA, replicaB: replicaB, other: other, all: []*loopbackInstance{replicaA, replicaB, other}}
	ctx, cancel := context.WithCancel(context.Background())
	for _, instance := range cluster.all {
		instance.plugin.ObserveChain([]string{firstAddress, secondAddress}, testHeight)
		if err := instance.plugin.Start(ctx); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		cancel()
		for _, instance := range cluster.all {
			instance.plugin.Wait()
		}
	})
	cluster.settleRegistry(ctx)
	return cluster
}

func (c *loopbackCluster) settleRegistry(ctx context.Context) {
	for range 2 {
		for _, instance := range c.all {
			instance.plugin.refreshRegistry(ctx)
		}
	}
}

func (c *loopbackCluster) pollAll(ctx context.Context) {
	for _, instance := range c.all {
		instance.plugin.pollPeers(ctx)
	}
}

func oneHostLoad(inflight int64) func() []LoadSample {
	return func() []LoadSample {
		return []LoadSample{{Participant: testHost, Model: testModel, InflightRequests: inflight}}
	}
}

// Test flow:
//  1. Start three instances over real mTLS: two replicas of one address and one of another; each registers itself through the registrar.
//  2. Poll for three rounds, the clock moving between them.
//  3. Assert each sees exactly the others, and has fresh data from them.
//  4. Assert no instance ever dialled its own address and each peer connection was dialled once, so the rounds reused it.
func TestThreeInstancesSeeExactlyTheOthersAndNeverThemselves(t *testing.T) {
	cluster := newLoopbackCluster(t, oneHostLoad(3), fixedConcurrency(10))
	ctx := context.Background()

	for range 3 {
		cluster.pollAll(ctx)
		cluster.clock.advance(loadInterval)
	}
	cluster.clock.advance(-loadInterval)

	for _, instance := range cluster.all {
		var want []string
		for _, other := range cluster.all {
			if other != instance {
				want = append(want, other.id.String())
			}
		}
		slices.Sort(want)
		if got := instanceNames(instance.plugin.members.Peers()); !slices.Equal(got, want) {
			t.Errorf("%s Peers() = %v, want %v", instance.id, got, want)
		}
		if got := peerNames(instance.plugin.View().freshPeers()); !slices.Equal(got, want) {
			t.Errorf("%s FreshPeers() = %v, want %v", instance.id, got, want)
		}
		if got := instance.dialer.count(instance.address); got != 0 {
			t.Errorf("%s dialled itself %d times", instance.id, got)
		}
		for _, other := range cluster.all {
			if other != instance {
				if got := instance.dialer.count(other.address); got != 1 {
					t.Errorf("%s dialled %s %d times, want one persistent connection", instance.id, other.id, got)
				}
			}
		}
		if got := instance.plugin.HandshakeCounts()[HandshakeAccepted]; got == 0 {
			t.Errorf("%s accepted no handshake", instance.id)
		}
	}
}

// Test flow:
//  1. Start three instances and poll once.
//  2. Remove one address from the allowlist of the two others, and run one registry refresh; the removed one keeps its old list.
//  3. Poll, then move the clock past the data's maximum age and poll again.
//  4. Assert the others dropped it within that one refresh, the view forgot it, and it gets no data from them any more.
func TestPeerRemovedFromTheAllowlistIsDroppedWithinOneRefresh(t *testing.T) {
	cluster := newLoopbackCluster(t, oneHostLoad(3), fixedConcurrency(10))
	ctx := context.Background()
	cluster.pollAll(ctx)

	for _, instance := range []*loopbackInstance{cluster.replicaA, cluster.replicaB} {
		instance.plugin.ObserveChain([]string{firstAddress}, testHeight)
		instance.plugin.refreshRegistry(ctx)
	}

	for _, instance := range []*loopbackInstance{cluster.replicaA, cluster.replicaB} {
		for _, peer := range instance.plugin.members.Peers() {
			if peer.ID.Address == secondAddress {
				t.Fatalf("%s still lists %s", instance.id, peer.ID)
			}
		}
		for _, fresh := range instance.plugin.View().freshPeers() {
			if fresh.Address == secondAddress {
				t.Fatalf("%s view still holds %s", instance.id, fresh)
			}
		}
	}

	cluster.clock.advance(loadMaxAge + time.Millisecond)
	for range redialRounds {
		cluster.other.plugin.pollPeers(ctx)
		if cluster.replicaA.plugin.HandshakeCounts()[HandshakeNotAllowed] > 0 {
			break
		}
	}
	if got := cluster.replicaA.plugin.HandshakeCounts()[HandshakeNotAllowed]; got == 0 {
		t.Fatal("the removed instance kept its connection: no new handshake reached the refusal")
	}
	if got := cluster.other.plugin.View().freshPeers(); len(got) != 0 {
		t.Fatalf("removed instance still receives from %v", got)
	}
}

// Test flow:
//  1. Start three instances.
//  2. Overwrite one instance's registry entry with a fingerprint of a key it does not hold, refuse its attempts to rewrite it, and let the others refresh.
//  3. Poll.
//  4. Assert the two others refuse it and it refuses them, as a fingerprint mismatch, while they still exchange with each other.
func TestPeerWithAWrongPinIsRefused(t *testing.T) {
	cluster := newLoopbackCluster(t, oneHostLoad(3), fixedConcurrency(10))
	ctx := context.Background()
	stolen := newIdentity(t, cluster.other.id)
	entry := registryEntry("other", stolen, "https://"+cluster.other.address)
	cluster.registry.forgeEntry(secondAddress, entry)
	for _, instance := range []*loopbackInstance{cluster.replicaA, cluster.replicaB} {
		instance.plugin.refreshRegistry(ctx)
	}

	cluster.replicaA.plugin.pollPeers(ctx)
	cluster.replicaB.plugin.pollPeers(ctx)
	cluster.other.plugin.pollPeers(ctx)

	want := []string{cluster.replicaB.id.String()}
	if got := peerNames(cluster.replicaA.plugin.View().freshPeers()); !slices.Equal(got, want) {
		t.Errorf("replica-a FreshPeers() = %v, want %v", got, want)
	}
	if cluster.replicaA.plugin.HandshakeCounts()[HandshakeFingerprint] == 0 {
		t.Error("replica-a recorded no fingerprint refusal")
	}
	if !cluster.replicaA.failures.contains(errFingerprint) {
		t.Error("replica-a's poll of the mispinned instance did not fail on the fingerprint")
	}
	if got := cluster.other.plugin.View().freshPeers(); len(got) != 0 {
		t.Errorf("the mispinned instance received from %v", got)
	}
}

// Test flow:
//  1. Open an mTLS client to one instance with a pinned peer's identity.
//  2. Fetch the load topic and an unknown topic over it.
//  3. Assert HTTP/2 over TLS 1.3 and the contract's status for each.
func TestChannelSpeaksHTTP2OverTLS13(t *testing.T) {
	cluster := newLoopbackCluster(t, oneHostLoad(3), fixedConcurrency(10))
	ctx := context.Background()
	cluster.replicaA.plugin.poller.Sync()
	link := cluster.replicaA.plugin.poller.links[cluster.other.id]
	if link == nil {
		t.Fatal("no link to the other instance")
	}

	type fetched struct {
		status     int
		protoMajor int
		tlsVersion uint16
		peerError  string
	}
	fetch := func(topic string) fetched {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+cluster.other.address+peerPathPrefix+topic, http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		response, err := link.client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = response.Body.Close() }()
		return fetched{status: response.StatusCode, protoMajor: response.ProtoMajor, tlsVersion: response.TLS.Version, peerError: response.Header.Get(headerPeerError)}
	}

	loaded := fetch(loadTopicName)
	if loaded.status != http.StatusOK || loaded.protoMajor != 2 || loaded.tlsVersion != tls.VersionTLS13 {
		t.Fatalf("load: %+v", loaded)
	}
	absent := fetch("absent")
	if absent.status != http.StatusNotFound || absent.peerError != peerErrorUnknownTopic {
		t.Fatalf("absent: %+v", absent)
	}
}

// Test flow:
//  1. Call every entry point the gateway calls on a nil plugin.
//  2. Assert no panic and no error: off is nothing to start, observe or wait for.
func TestNilPluginIsTheSwitchedOffFeature(t *testing.T) {
	var plugin *Plugin

	plugin.ObserveChain([]string{firstAddress}, testHeight)
	if err := plugin.Start(context.Background()); err != nil {
		t.Fatalf("Start() = %v", err)
	}
	plugin.Wait()
}

// Test flow:
//  1. Build a plugin; do not start it.
//  2. Assert its registry was not read, nothing was registered, nothing dialled.
//  3. Assert New refuses a bad instance id, advertise URL and key.
func TestNewTouchesNothingAndValidatesItsSettings(t *testing.T) {
	registry := newFakeRegistry()
	instance := newLoopbackInstance(t, PeerID{Address: firstAddress, Instance: "idle"}, registry, newFakeClock(), oneHostLoad(1), fixedConcurrency(1))
	t.Cleanup(func() { _ = instance.listener.Close() })

	if registry.callCount() != 0 || registry.registrationCount() != 0 || len(instance.dialer.dials) != 0 {
		t.Fatalf("New() touched the outside: %d reads, %d writes, %d dials", registry.callCount(), registry.registrationCount(), len(instance.dialer.dials))
	}

	good := Settings{Self: PeerID{Address: firstAddress, Instance: "good"}, AdvertiseURL: "https://good.example:8443", TransportKeyPEM: newKeyPEM(t)}
	deps := Deps{Registry: registry, Registrar: registry.registrar(firstAddress), Now: newFakeClock().now}
	if _, err := New(good, deps); err != nil {
		t.Fatalf("New(good) = %v", err)
	}
	badInstance, badURL, badKey := good, good, good
	badInstance.Self.Instance = "UPPER"
	badURL.AdvertiseURL = "http://plain.example:8443"
	badKey.TransportKeyPEM = "not a key"
	for name, settings := range map[string]Settings{"instance": badInstance, "url": badURL, "key": badKey} {
		if _, err := New(settings, deps); !errors.Is(err, ErrBadSettings) {
			t.Errorf("New(bad %s) = %v, want ErrBadSettings", name, err)
		}
	}
}

// Test flow:
//  1. Register this instance at a height, then observe heights inside and past the re-announce span.
//  2. Change the fingerprint on chain.
//  3. Assert a write happens only when the entry aged or differs, and never for an address outside the allowlist.
func TestAnnounceWritesOnlyWhenTheEntryNeedsIt(t *testing.T) {
	registry := newFakeRegistry()
	instance := newLoopbackInstance(t, PeerID{Address: firstAddress, Instance: "self"}, registry, newFakeClock(), oneHostLoad(1), fixedConcurrency(1))
	t.Cleanup(func() { _ = instance.listener.Close() })
	plugin := instance.plugin
	ctx := context.Background()

	plugin.members.ObserveChain([]string{secondAddress}, testHeight)
	plugin.refreshRegistry(ctx)
	if got := registry.registrationCount(); got != 0 {
		t.Fatalf("registered %d times outside the allowlist", got)
	}

	allowed := []string{firstAddress}
	plugin.members.ObserveChain(allowed, testHeight)
	plugin.refreshRegistry(ctx)
	plugin.refreshRegistry(ctx)
	if got := registry.registrationCount(); got != 1 {
		t.Fatalf("registrations = %d, want 1 while the entry is current", got)
	}
	fingerprint := plugin.identity.Fingerprint
	if registry.registered[0].SPKISHA256 != hex.EncodeToString(fingerprint[:]) || registry.registered[0].Topics[0] != "load@1" || registry.registered[0].Protocol != protocolVersion {
		t.Fatalf("registration = %+v", registry.registered[0])
	}

	plugin.members.ObserveChain(allowed, testHeight+reannounceBlocks)
	plugin.refreshRegistry(ctx)
	if got := registry.registrationCount(); got != 2 {
		t.Fatalf("registrations = %d, want a second once aged", got)
	}

	plugin.members.ObserveChain(allowed, testHeight)
	entry := registryEntry("self", newIdentity(t, instance.id), "https://"+instance.address)
	registry.put(firstAddress, entry)
	plugin.refreshRegistry(ctx)
	if got := registry.registrationCount(); got != 3 {
		t.Fatalf("registrations = %d, want a third after the fingerprint differed", got)
	}
}

type forgedSigner struct{ public crypto.PublicKey }

func (s forgedSigner) Public() crypto.PublicKey { return s.public }

func (s forgedSigner) Sign(io.Reader, []byte, crypto.SignerOpts) ([]byte, error) {
	return make([]byte, ed25519.SignatureSize), nil
}

// Test flow:
//  1. Open a TLS client to one instance presenting another instance's public certificate with a forged signature.
//  2. Assert the connection is refused and the accepted count did not move.
//  3. Poll once from every instance and assert each counts one accepted handshake per connection on each side.
func TestAcceptedCountsOnlyCompletedHandshakes(t *testing.T) {
	cluster := newLoopbackCluster(t, oneHostLoad(3), fixedConcurrency(10))
	victim := cluster.replicaB.plugin.identity.Certificate
	var dialer net.Dialer
	raw, err := dialer.DialContext(context.Background(), "tcp", cluster.replicaA.address)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	client := tls.Client(raw, &tls.Config{
		MinVersion: tls.VersionTLS13, InsecureSkipVerify: true, NextProtos: []string{http2Protocol}, //nolint:gosec
		Certificates: []tls.Certificate{{Certificate: victim.Certificate, Leaf: victim.Leaf, PrivateKey: forgedSigner{public: victim.Leaf.PublicKey}}},
	})
	_ = client.HandshakeContext(context.Background())
	if _, err := client.Read(make([]byte, 1)); err == nil {
		t.Fatal("the listener kept a connection with a forged signature")
	}
	if got := cluster.replicaA.plugin.HandshakeCounts()[HandshakeAccepted]; got != 0 {
		t.Fatalf("accepted = %d after a handshake that never completed", got)
	}

	cluster.pollAll(context.Background())

	for _, instance := range cluster.all {
		if got := instance.plugin.HandshakeCounts()[HandshakeAccepted]; got != 4 {
			t.Errorf("%s accepted = %d, want 4: two peers dialled and two dialling", instance.id, got)
		}
	}
}

package peerload

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/url"
	"testing"
)

type signerKey = crypto.Signer

type verifierFixture struct {
	registry *fakeRegistry
	clock    *fakeClock
	members  *membership
	verifier *verifier
	selfID   transportIdentity
	peerID   PeerID
	peer     transportIdentity
}

func newVerifierFixture(t *testing.T) *verifierFixture {
	t.Helper()
	registry := newFakeRegistry()
	clock := newFakeClock()
	members, selfIdentity := newTestMembership(t, registry, clock)
	peerID := PeerID{Address: secondAddress, Instance: "other"}
	peer := newIdentity(t, peerID)
	registry.put(secondAddress, registryEntry("other", peer, "https://other.example:8443"))
	members.ObserveChain([]string{firstAddress, secondAddress}, testHeight)
	refreshed(t, members)
	return &verifierFixture{registry: registry, clock: clock, members: members, verifier: newVerifier(members), selfID: selfIdentity, peerID: peerID, peer: peer}
}

func certificateWithURIs(t *testing.T, base transportIdentity, uris ...*url.URL) *x509.Certificate {
	t.Helper()
	signer := base.Certificate.PrivateKey.(signerKey)
	template := &x509.Certificate{SerialNumber: base.Certificate.Leaf.SerialNumber, URIs: uris}
	der, err := x509.CreateCertificate(rand.Reader, template, template, signer.Public(), signer)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return certificate
}

// Test flow:
//  1. Build a registry with one pinned peer.
//  2. Present the pinned certificate.
//  3. Assert it passes, and is not counted as accepted before its handshake completes.
func TestVerifierAcceptsThePinnedKeyOfAnAllowlistedInstance(t *testing.T) {
	fixture := newVerifierFixture(t)

	if err := fixture.verifier.VerifyConnection(stateOf(fixture.peer)); err != nil {
		t.Fatalf("VerifyConnection() = %v", err)
	}
	if got := fixture.verifier.HandshakeCounts()[HandshakeAccepted]; got != 0 {
		t.Fatalf("accepted count = %d, want 0 until the handshake completes", got)
	}
}

// Test flow:
//  1. Present a certificate under the pinned instance's name carrying another key.
//  2. Assert it is refused as a fingerprint mismatch after one registry reread.
func TestVerifierRefusesTheWrongKeyAfterOneReread(t *testing.T) {
	fixture := newVerifierFixture(t)
	before := fixture.registry.callCount()

	err := fixture.verifier.VerifyConnection(stateOf(newIdentity(t, fixture.peerID)))

	if !errors.Is(err, errFingerprint) {
		t.Fatalf("VerifyConnection() = %v, want errFingerprint", err)
	}
	if got := fixture.registry.callCount() - before; got != 1 {
		t.Fatalf("registry rereads = %d, want 1", got)
	}
}

// Test flow:
//  1. Remove the peer's address from the allowlist.
//  2. Present its pinned certificate.
//  3. Assert it is refused without any registry read.
func TestVerifierRefusesAnAddressOutsideTheAllowlist(t *testing.T) {
	fixture := newVerifierFixture(t)
	fixture.members.ObserveChain([]string{firstAddress}, testHeight)
	before := fixture.registry.callCount()

	err := fixture.verifier.VerifyConnection(stateOf(fixture.peer))

	if !errors.Is(err, errNotAllowed) {
		t.Fatalf("VerifyConnection() = %v, want errNotAllowed", err)
	}
	if fixture.registry.callCount() != before {
		t.Fatal("a refused address caused a registry read")
	}
}

// Test flow:
//  1. Present certificates with no gonka SAN, a SAN without an instance, a SAN with an invalid instance, two gonka SANs, and a gonka SAN beside another URI.
//  2. Present no certificate, and two certificates.
//  3. Assert each is refused as an identity failure.
func TestVerifierRefusesACertificateThatDoesNotNameOneInstance(t *testing.T) {
	fixture := newVerifierFixture(t)
	gonkaURI := func(opaque string) *url.URL { return &url.URL{Scheme: sanScheme, Opaque: opaque} }
	cases := map[string]tls.ConnectionState{
		"no gonka uri":     {PeerCertificates: []*x509.Certificate{certificateWithURIs(t, fixture.peer, &url.URL{Scheme: "https", Host: "other.example"})}},
		"no instance":      {PeerCertificates: []*x509.Certificate{certificateWithURIs(t, fixture.peer, gonkaURI(secondAddress))}},
		"invalid instance": {PeerCertificates: []*x509.Certificate{certificateWithURIs(t, fixture.peer, gonkaURI(secondAddress+"/UPPER"))}},
		"two instances":    {PeerCertificates: []*x509.Certificate{certificateWithURIs(t, fixture.peer, gonkaURI(fixture.peerID.String()), gonkaURI(secondAddress+"/second"))}},
		"another uri":      {PeerCertificates: []*x509.Certificate{certificateWithURIs(t, fixture.peer, gonkaURI(fixture.peerID.String()), &url.URL{Scheme: "https", Host: "other.example"})}},
		"no certificate":   {},
		"two certificates": {PeerCertificates: []*x509.Certificate{fixture.peer.Certificate.Leaf, fixture.peer.Certificate.Leaf}},
	}
	for name, state := range cases {
		t.Run(name, func(t *testing.T) {
			if err := fixture.verifier.VerifyConnection(state); !errors.Is(err, errIdentity) {
				t.Fatalf("VerifyConnection() = %v, want errIdentity", err)
			}
		})
	}
}

// Test flow:
//  1. Present a replica's key under a sibling's name: the SAN and the pin disagree.
//  2. Assert it is refused as a fingerprint mismatch.
func TestVerifierRefusesAnotherInstancesKeyUnderTheSameAddress(t *testing.T) {
	fixture := newVerifierFixture(t)
	sibling := PeerID{Address: secondAddress, Instance: "sibling"}
	fixture.registry.put(secondAddress, registryEntry("sibling", newIdentity(t, sibling), "https://sibling.example:8443"))
	refreshed(t, fixture.members)

	forged, err := issueIdentity(fixture.peer.Certificate.PrivateKey.(signerKey), sibling)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.verifier.VerifyConnection(stateOf(forged)); !errors.Is(err, errFingerprint) {
		t.Fatalf("VerifyConnection() = %v, want errFingerprint", err)
	}
}

// Test flow:
//  1. A new instance connects before this gateway has read its entry; the entry is already on chain.
//  2. A second unknown instance connects within the rate limit.
//  3. Assert the first is accepted through the reread, and the second is refused without another read.
func TestVerifierRereadsTheRegistryForAnUnknownInstanceUnderTheRateLimit(t *testing.T) {
	fixture := newVerifierFixture(t)
	newcomerID := PeerID{Address: secondAddress, Instance: "newcomer"}
	newcomer := newIdentity(t, newcomerID)
	fixture.registry.put(secondAddress, registryEntry("newcomer", newcomer, "https://newcomer.example:8443"))
	before := fixture.registry.callCount()

	if err := fixture.verifier.VerifyConnection(stateOf(newcomer)); err != nil {
		t.Fatalf("VerifyConnection(newcomer) = %v", err)
	}
	unknown := newIdentity(t, PeerID{Address: secondAddress, Instance: "unknown"})
	if err := fixture.verifier.VerifyConnection(stateOf(unknown)); !errors.Is(err, errFingerprint) {
		t.Fatalf("VerifyConnection(unknown) = %v, want errFingerprint", err)
	}
	if got := fixture.registry.callCount() - before; got != 1 {
		t.Fatalf("registry rereads = %d, want 1", got)
	}
}

// Test flow:
//  1. Present this instance's own name, then its own key under a replica's name.
//  2. Assert both are refused as self.
func TestVerifierRefusesThisInstanceByNameAndByKey(t *testing.T) {
	fixture := newVerifierFixture(t)
	sameName := newIdentity(t, fixture.members.self.ID)
	sameKey, err := issueIdentity(fixture.selfID.Certificate.PrivateKey.(signerKey), PeerID{Address: firstAddress, Instance: "stale-name"})
	if err != nil {
		t.Fatal(err)
	}

	for name, identity := range map[string]transportIdentity{"name": sameName, "key": sameKey} {
		if err := fixture.verifier.VerifyConnection(stateOf(identity)); !errors.Is(err, errSelf) {
			t.Fatalf("VerifyConnection(%s) = %v, want errSelf", name, err)
		}
	}
}

// Test flow:
//  1. Register an instance that declares a protocol this gateway does not speak.
//  2. Present its pinned certificate.
//  3. Assert it is refused as a protocol failure.
func TestVerifierRefusesAnUnsupportedProtocol(t *testing.T) {
	fixture := newVerifierFixture(t)
	entry := registryEntry("other", fixture.peer, "https://other.example:8443")
	entry.Protocol = protocolVersion + 1
	fixture.registry.put(secondAddress, entry)
	refreshed(t, fixture.members)

	if err := fixture.verifier.VerifyConnection(stateOf(fixture.peer)); !errors.Is(err, errProtocol) {
		t.Fatalf("VerifyConnection() = %v, want errProtocol", err)
	}
}

// Test flow:
//  1. Build a client config dialling one instance.
//  2. Present another valid instance's pinned certificate to it.
//  3. Assert it is refused as the wrong peer.
func TestClientRefusesAValidPeerThatIsNotTheOneDialled(t *testing.T) {
	fixture := newVerifierFixture(t)
	config := fixture.verifier.ClientTLSConfig(fixture.selfID, PeerID{Address: secondAddress, Instance: "dialled"})

	if err := config.VerifyConnection(stateOf(fixture.peer)); !errors.Is(err, errWrongPeer) {
		t.Fatalf("VerifyConnection() = %v, want errWrongPeer", err)
	}
}

func handshakeOverPipe(serverConfig, clientConfig *tls.Config) (serverErr, clientErr error) {
	serverEnd, clientEnd := net.Pipe()
	defer func() { _ = serverEnd.Close() }()
	defer func() { _ = clientEnd.Close() }()
	serverDone := make(chan error, 1)
	go func() {
		err := tls.Server(serverEnd, serverConfig).HandshakeContext(context.Background())
		if err != nil {
			_ = serverEnd.Close()
		}
		serverDone <- err
	}()
	clientErr = tls.Client(clientEnd, clientConfig).HandshakeContext(context.Background())
	if clientErr != nil {
		_ = clientEnd.Close()
	}
	return <-serverDone, clientErr
}

// Test flow:
//  1. Offer the listener's config a pinned peer's client that speaks at most TLS 1.2.
//  2. Assert the listener refuses the handshake.
func TestListenerRefusesTLS12(t *testing.T) {
	fixture := newVerifierFixture(t)
	client := &tls.Config{MaxVersion: tls.VersionTLS12, Certificates: []tls.Certificate{fixture.peer.Certificate}, InsecureSkipVerify: true, NextProtos: []string{http2Protocol}} //nolint:gosec

	serverErr, _ := handshakeOverPipe(fixture.verifier.ServerTLSConfig(fixture.selfID), client)

	if serverErr == nil {
		t.Fatal("the listener completed a TLS 1.2 handshake")
	}
}

// Test flow:
//  1. Dial the pinned peer through the client config while the peer speaks at most TLS 1.2.
//  2. Assert the client refuses the handshake.
func TestClientRefusesTLS12(t *testing.T) {
	fixture := newVerifierFixture(t)
	server := &tls.Config{MaxVersion: tls.VersionTLS12, Certificates: []tls.Certificate{fixture.peer.Certificate}, NextProtos: []string{http2Protocol}} //nolint:gosec

	_, clientErr := handshakeOverPipe(server, fixture.verifier.ClientTLSConfig(fixture.selfID, fixture.peerID))

	if clientErr == nil {
		t.Fatal("the client completed a TLS 1.2 handshake")
	}
}

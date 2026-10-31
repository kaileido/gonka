package peerload

import (
	"context"
	"crypto/tls"
	"errors"
	"maps"
	"sync"
)

type verifier struct {
	members *membership

	mu         sync.Mutex
	handshakes map[string]uint64
}

func newVerifier(members *membership) *verifier {
	return &verifier{members: members, handshakes: map[string]uint64{}}
}

// VerifyConnection refuses a peer that fails the check and counts the refusal; an unknown instance or fingerprint rereads the registry first, rate limited.
func (v *verifier) VerifyConnection(state tls.ConnectionState) error {
	_, err := v.verify(state)
	if err != nil {
		v.count(err)
	}
	return err
}

func (v *verifier) countAccepted() { v.count(nil) }

// Authorize repeats the check without touching the registry, so a request on a connection whose peer was removed is refused at once.
func (v *verifier) Authorize(state *tls.ConnectionState) (PeerID, error) {
	if state == nil {
		return PeerID{}, errIdentity
	}
	return v.check(*state)
}

// Listed reports whether the instance is still in the allowlist and the registry.
func (v *verifier) Listed(id PeerID) bool {
	if !v.members.Allowed(id.Address) {
		return false
	}
	_, known := v.members.Lookup(id)
	return known
}

// HandshakeCounts copies the handshake results so far, by result.
func (v *verifier) HandshakeCounts() map[string]uint64 {
	v.mu.Lock()
	defer v.mu.Unlock()
	return maps.Clone(v.handshakes)
}

// ServerTLSConfig is the listener's config: TLS 1.3, a client certificate required, no resumption, so every connection runs VerifyConnection in full.
func (v *verifier) ServerTLSConfig(identity transportIdentity) *tls.Config {
	return &tls.Config{
		MinVersion:             tls.VersionTLS13,
		Certificates:           []tls.Certificate{identity.Certificate},
		ClientAuth:             tls.RequireAnyClientCert,
		VerifyConnection:       v.VerifyConnection,
		NextProtos:             []string{http2Protocol},
		SessionTicketsDisabled: true,
	}
}

// ClientTLSConfig dials one instance: the certificate must carry the registry pin and name the instance dialled.
func (v *verifier) ClientTLSConfig(identity transportIdentity, dialled PeerID) *tls.Config {
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{identity.Certificate},
		// Trust is the registry pin checked in VerifyConnection; self-signed transport keys have no CA chain to verify.
		InsecureSkipVerify: true, //nolint:gosec
		NextProtos:         []string{http2Protocol},
		VerifyConnection: func(state tls.ConnectionState) error {
			id, err := v.verify(state)
			if err == nil && id != dialled {
				err = errWrongPeer
			}
			if err != nil {
				v.count(err)
			}
			return err
		},
	}
}

func (v *verifier) verify(state tls.ConnectionState) (PeerID, error) {
	id, err := v.check(state)
	if !errors.Is(err, errFingerprint) {
		return id, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), refreshTimeout)
	defer cancel()
	if refreshErr := v.members.RefreshOne(ctx, id.Address); refreshErr != nil {
		return id, errors.Join(err, refreshErr)
	}
	return v.check(state)
}

func (v *verifier) check(state tls.ConnectionState) (PeerID, error) {
	if len(state.PeerCertificates) != 1 {
		return PeerID{}, errIdentity
	}
	certificate := state.PeerCertificates[0]
	id, err := peerIDFromCertificate(certificate)
	if err != nil {
		return PeerID{}, err
	}
	fingerprint := fingerprintOf(certificate)
	if id == v.members.self.ID || fingerprint == v.members.self.Fingerprint {
		return id, errSelf
	}
	if !v.members.Allowed(id.Address) {
		return id, errNotAllowed
	}
	instance, known := v.members.Lookup(id)
	if !known || instance.Fingerprint != fingerprint {
		return id, errFingerprint
	}
	if instance.Protocol != protocolVersion {
		return id, errProtocol
	}
	return id, nil
}

func (v *verifier) count(err error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.handshakes[handshakeResultOf(err)]++
}

func handshakeResultOf(err error) string {
	switch {
	case err == nil:
		return HandshakeAccepted
	case errors.Is(err, errSelf):
		return HandshakeSelf
	case errors.Is(err, errNotAllowed):
		return HandshakeNotAllowed
	case errors.Is(err, errFingerprint):
		return HandshakeFingerprint
	case errors.Is(err, errProtocol):
		return HandshakeProtocol
	default:
		return HandshakeIdentity
	}
}

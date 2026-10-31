package peerload

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"testing"
)

const pinnedTransportPublicKey = "5128e318d34e2f3144305b99dda8d86cc570f7d21f96329b2a9cdf89588517c0"

func deriveForTest(t *testing.T, chainKey []byte, id PeerID) transportIdentity {
	t.Helper()
	identity, err := deriveTransportIdentity(chainKey, id)
	if err != nil {
		t.Fatal(err)
	}
	return identity
}

// Test flow:
//  1. Derive twice from one chain key and one instance.
//  2. Assert the key and the fingerprint are the same, as after a restart.
func TestTransportKeyIsDeterministic(t *testing.T) {
	chainKey := newChainKey(t)
	id := PeerID{Address: firstAddress, Instance: "self"}

	first := deriveForTest(t, chainKey, id)
	second := deriveForTest(t, chainKey, id)

	if first.Fingerprint != second.Fingerprint {
		t.Fatal("the fingerprint moved between two derivations")
	}
	firstKey, secondKey := first.Certificate.PrivateKey.(ed25519.PrivateKey), second.Certificate.PrivateKey.(ed25519.PrivateKey)
	if !firstKey.Equal(secondKey) {
		t.Fatal("the key moved between two derivations")
	}
}

// Test flow:
//  1. Pin the derivation of one fixed chain key and instance to its known public key.
//  2. Assert it, so a changed label or info cannot pass unnoticed: it would change every registered fingerprint.
func TestTransportKeyDerivationIsPinned(t *testing.T) {
	chainKey := make([]byte, chainKeyBytes)
	for index := range chainKey {
		chainKey[index] = 0x11
	}

	identity := deriveForTest(t, chainKey, PeerID{Address: firstAddress, Instance: "self"})

	public := identity.Certificate.PrivateKey.(ed25519.PrivateKey).Public().(ed25519.PublicKey)
	if got := hex.EncodeToString(public); got != pinnedTransportPublicKey {
		t.Fatalf("public key = %s, want %s", got, pinnedTransportPublicKey)
	}
}

// Test flow:
//  1. Derive for one instance, for another instance id, for another address and from another chain key.
//  2. Assert every fingerprint differs from the first.
func TestTransportKeyDiffersByInstanceAddressAndChainKey(t *testing.T) {
	chainKey := newChainKey(t)
	base := deriveForTest(t, chainKey, PeerID{Address: firstAddress, Instance: "self"})

	testCases := []struct {
		name  string
		other transportIdentity
	}{
		{"instance", deriveForTest(t, chainKey, PeerID{Address: firstAddress, Instance: "other"})},
		{"address", deriveForTest(t, chainKey, PeerID{Address: secondAddress, Instance: "self"})},
		{"chain key", deriveForTest(t, newChainKey(t), PeerID{Address: firstAddress, Instance: "self"})},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			if testCase.other.Fingerprint == base.Fingerprint {
				t.Fatalf("deriveTransportIdentity() = %x, want another fingerprint", testCase.other.Fingerprint)
			}
		})
	}
}

// Test flow:
//  1. Build a plugin from a chain key, allowlist its address and announce it.
//  2. Compute the SPKI fingerprint of the key derived for it, from the key itself.
//  3. Assert that is what the plugin registered, and that the plugin kept no chain key.
func TestRegisteredFingerprintIsTheDerivedKeysSPKI(t *testing.T) {
	registry := newFakeRegistry()
	chainKey := newChainKey(t)
	self := PeerID{Address: firstAddress, Instance: "self"}
	plugin, err := New(Settings{Self: self, AdvertiseURL: selfEndpoint, ChainKey: chainKey, Rounding: DefaultRounding()}, Deps{
		Registry:  registry,
		Registrar: registry.registrar(firstAddress),
		Now:       newFakeClock().now,
	})
	if err != nil {
		t.Fatal(err)
	}
	plugin.ObserveChain([]string{firstAddress}, testHeight)
	if err := plugin.members.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}

	if err := plugin.announce(t.Context()); err != nil {
		t.Fatal(err)
	}

	if plugin.settings.ChainKey != nil {
		t.Fatal("plugin.settings.ChainKey is held after New, want nil")
	}
	key := deriveForTest(t, chainKey, self).Certificate.PrivateKey.(ed25519.PrivateKey)
	spki, err := x509.MarshalPKIXPublicKey(key.Public())
	if err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(spki)
	if registry.registrationCount() != 1 || registry.registered[0].SPKISHA256 != hex.EncodeToString(want[:]) {
		t.Fatalf("registered = %+v, want the fingerprint %x", registry.registered, want)
	}
}

// Test flow:
//  1. Derive from chain keys of the wrong length.
//  2. Assert ErrBadSettings.
func TestTransportKeyRefusesAChainKeyOfTheWrongLength(t *testing.T) {
	testCases := []struct {
		name     string
		chainKey []byte
	}{
		{"absent", nil},
		{"one byte short", make([]byte, chainKeyBytes-1)},
		{"one byte long", make([]byte, chainKeyBytes+1)},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := deriveTransportIdentity(testCase.chainKey, PeerID{Address: firstAddress, Instance: "self"}); !errors.Is(err, ErrBadSettings) {
				t.Fatalf("deriveTransportIdentity(%d bytes) = %v, want ErrBadSettings", len(testCase.chainKey), err)
			}
		})
	}
}

package peerload

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/url"
	"strings"
	"time"
)

const (
	certificateSerial      = 1
	certificateLastYear    = 9999
	certificateLastMonth   = time.December
	certificateLastDay     = 31
	certificateEpochSecond = 0
)

// PeerID names one gateway instance: the allowlisted address and the instance id under it.
type PeerID struct {
	Address  string
	Instance string
}

func (id PeerID) String() string { return id.Address + idSeparator + id.Instance }

type spkiFingerprint [sha256.Size]byte

type transportIdentity struct {
	Certificate tls.Certificate
	Fingerprint spkiFingerprint
}

func loadTransportIdentity(keyPEM string, self PeerID) (transportIdentity, error) {
	block, _ := pem.Decode([]byte(keyPEM))
	if block == nil {
		return transportIdentity{}, fmt.Errorf("%w: transport key is not PEM", ErrBadSettings)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return transportIdentity{}, fmt.Errorf("%w: transport key is not PKCS#8", ErrBadSettings)
	}
	var signer crypto.Signer
	switch key := parsed.(type) {
	case ed25519.PrivateKey:
		signer = key
	case *ecdsa.PrivateKey:
		signer = key
	default:
		return transportIdentity{}, fmt.Errorf("%w: transport key must be ed25519 or ECDSA", ErrBadSettings)
	}
	return issueIdentity(signer, self)
}

func issueIdentity(signer crypto.Signer, self PeerID) (transportIdentity, error) {
	template := &x509.Certificate{
		SerialNumber: big.NewInt(certificateSerial),
		NotBefore:    time.Unix(certificateEpochSecond, 0),
		NotAfter:     time.Date(certificateLastYear, certificateLastMonth, certificateLastDay, 0, 0, 0, 0, time.UTC),
		URIs:         []*url.URL{{Scheme: sanScheme, Opaque: self.String()}},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, signer.Public(), signer)
	if err != nil {
		return transportIdentity{}, fmt.Errorf("issue transport certificate: %w", err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		return transportIdentity{}, fmt.Errorf("parse transport certificate: %w", err)
	}
	return transportIdentity{
		Certificate: tls.Certificate{Certificate: [][]byte{der}, PrivateKey: signer, Leaf: certificate},
		Fingerprint: fingerprintOf(certificate),
	}, nil
}

func fingerprintOf(certificate *x509.Certificate) spkiFingerprint {
	return sha256.Sum256(certificate.RawSubjectPublicKeyInfo)
}

func peerIDFromCertificate(certificate *x509.Certificate) (PeerID, error) {
	if len(certificate.URIs) != 1 || certificate.URIs[0].Scheme != sanScheme {
		return PeerID{}, errIdentity
	}
	address, instance, separated := strings.Cut(certificate.URIs[0].Opaque, idSeparator)
	if !separated || address == emptyAddress || !validInstanceID(instance) {
		return PeerID{}, errIdentity
	}
	return PeerID{Address: address, Instance: instance}, nil
}

func validInstanceID(instance string) bool {
	if instance == "" || len(instance) > instanceIDLimit {
		return false
	}
	for _, character := range instance {
		isLetter := character >= 'a' && character <= 'z'
		isDigit := character >= '0' && character <= '9'
		if !isLetter && !isDigit && character != '-' {
			return false
		}
	}
	return true
}

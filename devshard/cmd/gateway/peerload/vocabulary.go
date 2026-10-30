package peerload

import (
	"errors"
	"time"
)

// See docs/peerload.md, "Channel contract", for every value below.
const (
	protocolVersion uint16 = 1

	peerPathPrefix    = "/v1/peer/"
	peerRoute         = peerPathPrefix + "{" + topicPathValue + "}"
	topicPathValue    = "topic"
	contentType       = "application/x-protobuf"
	headerPeerError   = "Gonka-Peer-Error"
	headerContentType = "Content-Type"
	headerAllow       = "Allow"

	peerErrorUnknownTopic  = "unknown_topic"
	peerErrorBodyForbidden = "body_forbidden"
	peerErrorRefused       = "refused"

	sanScheme       = "gonka"
	idSeparator     = "/"
	topicSplitter   = "@"
	httpsScheme     = "https"
	schemeSeparator = "://"
	portSeparator   = ':'
	http2Protocol   = "h2"
	listenNetwork   = "tcp"
	emptyAddress    = ""

	loadTopicName             = "load"
	loadTopicVersion   uint32 = 1
	loadMaxBytes              = 64 << 10
	loadInterval              = 10 * time.Second
	loadMaxAge                = 30 * time.Second
	envelopeOverhead          = 512
	maxLoadRows               = 512
	maxLoadModels             = 32
	maxLoadStringBytes        = 128

	defaultRoundRequests uint32 = 2
	defaultRoundTokens   uint32 = 2048

	registryRefreshInterval = 300 * time.Second
	refreshOnePerAddress    = 30 * time.Second
	refreshTimeout          = 5 * time.Second
	registryReadTimeout     = 10 * time.Second
	registryRefreshBudget   = 60 * time.Second
	firstReadRetryInterval  = 10 * time.Second
	registerBroadcastMargin = 15 * time.Second
	reregisterInterval      = 6 * time.Hour
	noWriteBackoff          = time.Duration(0)
	endpointLimit           = 256
	registryBatchAddresses  = 100
	maxInstancesPerAddress  = 16
	instanceTTLBlocks       = 16200
	reannounceBlocks        = 4050
	instanceIDLimit         = 32

	pollRequestTimeout  = 5 * time.Second
	serverHeaderTimeout = 5 * time.Second
	serverIdleTimeout   = 60 * time.Second
	maxHeaderBytes      = 16 << 10

	discountFloor   = 0.25
	discountNeutral = 1.0

	concurrencyUnknown int64 = 0

	HandshakeAccepted    = "accepted"
	HandshakeSelf        = "self"
	HandshakeIdentity    = "identity"
	HandshakeNotAllowed  = "not_allowed"
	HandshakeFingerprint = "fingerprint"
	HandshakeProtocol    = "protocol"
)

// Cutoff is the wire form of a host's cut-off state.
type Cutoff uint32

// The wire values of Cutoff.
const (
	CutoffClosed Cutoff = iota
	CutoffHalfOpen
	CutoffOpen
)

// The refusals the verifier and the envelope checks return.
var (
	errSelf          = errors.New("peerload: peer is this instance")
	errPartialRead   = errors.New("peerload: part of the allowlist could not be read")
	errIdentity      = errors.New("peerload: certificate does not carry one gonka identity")
	errNotAllowed    = errors.New("peerload: address is not in the allowlist")
	errFingerprint   = errors.New("peerload: key does not match the registry entry of the instance")
	errProtocol      = errors.New("peerload: instance speaks an unsupported protocol")
	errWrongPeer     = errors.New("peerload: certificate names another instance than the one dialled")
	errMalformed     = errors.New("peerload: malformed message")
	errTooLarge      = errors.New("peerload: message exceeds its cap")
	errEnvelope      = errors.New("peerload: envelope does not match the connection or the topic")
	errStaleEnvelope = errors.New("peerload: envelope is outside the accepted age")
	errReplay        = errors.New("peerload: envelope sequence did not increase")
	ErrBadSettings   = errors.New("peerload: invalid settings")
)

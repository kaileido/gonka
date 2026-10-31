package app

import (
	"context"
	"encoding/hex"
	"fmt"
	"net"
	"time"

	"common/httpguard"

	"devshard/cmd/gateway/chain"
	"devshard/cmd/gateway/config"
	"devshard/cmd/gateway/internal/logkey"
	"devshard/cmd/gateway/limits"
	"devshard/cmd/gateway/peerload"
	"devshard/cmd/gateway/perf"
	"devshard/logging"
	"devshard/signing"
)

const (
	peerSubsystem      = "peerload"
	unknownConcurrency = int64(0)
)

type contractRegistry interface {
	MembersOf(ctx context.Context, contract string, addresses []string) ([]chain.RegistryMember, error)
}

type instanceRegistrar interface {
	RegisterInstance(ctx context.Context, signer *signing.Secp256k1Signer, contract string, registration chain.RegistryRegistration) error
}

type signerSource interface {
	SignerFor(privateKeyEnv string) (*signing.Secp256k1Signer, error)
}

type peerChannelSources struct {
	registry     contractRegistry
	txClient     instanceRegistrar
	signers      signerSource
	participants *limits.ParticipantLimiter
	hosts        *perf.Tracker
	capacity     *limits.Capacity
	configHolder *config.Holder
	commitPoll   time.Duration
	now          func() time.Time
}

type boundRegistry struct {
	registry contractRegistry
	contract string
}

func (b boundRegistry) MembersOf(ctx context.Context, addresses []string) ([]chain.RegistryMember, error) {
	return b.registry.MembersOf(ctx, b.contract, addresses)
}

type boundRegistrar struct {
	registrar instanceRegistrar
	signer    *signing.Secp256k1Signer
	contract  string
}

func (b boundRegistrar) RegisterInstance(ctx context.Context, registration chain.RegistryRegistration) error {
	return b.registrar.RegisterInstance(ctx, b.signer, b.contract, registration)
}

func newPeerChannel(settings config.Peer, sources peerChannelSources) (*peerload.Plugin, error) {
	if !settings.Active() {
		return nil, nil
	}
	if sources.registry == nil {
		return nil, fmt.Errorf("%w: the channel needs the chain connection", peerload.ErrBadSettings)
	}
	signer, err := sources.signers.SignerFor(settings.IdentityKeyEnv)
	if err != nil {
		return nil, fmt.Errorf("peer identity key: %w", err)
	}
	chainKey, err := hex.DecodeString(signer.PrivateKeyHex())
	if err != nil {
		return nil, fmt.Errorf("decoding peer identity key: %w", err)
	}
	return peerload.New(peerload.Settings{
		Self:          peerload.PeerID{Address: signer.Address(), Instance: settings.InstanceID},
		AdvertiseURL:  settings.AdvertiseURL,
		ListenAddress: settings.Listen,
		ChainKey:      chainKey,
		Rounding:      peerload.DefaultRounding(),
		CommitPoll:    sources.commitPoll,
	}, peerDeps(settings, signer, sources))
}

func peerDeps(settings config.Peer, signer *signing.Secp256k1Signer, sources peerChannelSources) peerload.Deps {
	listener := net.ListenConfig{}
	return peerload.Deps{
		Registry:    boundRegistry{registry: sources.registry, contract: settings.RegistryContract},
		Registrar:   boundRegistrar{registrar: sources.txClient, signer: signer, contract: settings.RegistryContract},
		Samples:     func() []peerload.LoadSample { return localLoadSamples(sources) },
		Concurrency: func(participant, model string) int64 { return peerConcurrency(sources, participant, model) },
		Now:         sources.now,
		Listen:      listener.Listen,
		Dial:        httpguard.NewDialer().DialContext,
		Report: func(message string, err error) {
			logging.Warn(message, logkey.Subsystem, peerSubsystem, logkey.Error, err)
		},
	}
}

func peerConcurrency(sources peerChannelSources, participant, model string) int64 {
	if !sources.capacity.ModelKnown(model) {
		return unknownConcurrency
	}
	return limits.EarnedConcurrency(sources.configHolder.Load().Limits.Concurrency.RequestsPer10000Weight, sources.capacity.ParticipantWeight(participant, model))
}

func localLoadSamples(sources peerChannelSources) []peerload.LoadSample {
	return joinLoadSamples(sources.participants.Snapshot(), sources.hosts.Snapshot(), sources.hosts.Pressure)
}

func joinLoadSamples(windows []limits.HostWindow, hosts []perf.HostState, pressure func(participant, model string) perf.Pressure) []peerload.LoadSample {
	type hostPair struct{ participant, model string }
	samples := make(map[hostPair]*peerload.LoadSample, len(windows))
	sampleOf := func(participant, model string) *peerload.LoadSample {
		key := hostPair{participant: participant, model: model}
		sample, known := samples[key]
		if !known {
			sample = &peerload.LoadSample{Participant: participant, Model: model}
			samples[key] = sample
		}
		return sample
	}
	for _, window := range windows {
		sample := sampleOf(window.Participant, window.Model)
		sample.InflightInputTokens = window.InflightInputTokens
		sample.InflightOutputTokens = window.InflightOutputTokens
		sample.Cutoff = cutoffOf(window.Cutoff)
	}
	for _, host := range hosts {
		sample := sampleOf(host.Participant, host.Model)
		sample.InflightRequests = int64(host.ModelInflight)
		sample.Ejected = host.Ejected
	}
	joined := make([]peerload.LoadSample, 0, len(samples))
	for _, sample := range samples {
		measured := pressure(sample.Participant, sample.Model)
		sample.PressureFirstContent = measured.FirstContent
		sample.PressureDecode = measured.Decode
		joined = append(joined, *sample)
	}
	return joined
}

func cutoffOf(state limits.CutoffState) peerload.Cutoff {
	switch state {
	case limits.CutoffOpen:
		return peerload.CutoffOpen
	case limits.CutoffHalfOpen:
		return peerload.CutoffHalfOpen
	default:
		return peerload.CutoffClosed
	}
}

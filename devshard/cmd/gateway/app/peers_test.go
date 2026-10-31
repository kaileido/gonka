package app

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"common/httpguard"

	"devshard/cmd/gateway/chain"
	"devshard/cmd/gateway/config"
	"devshard/cmd/gateway/limits"
	"devshard/cmd/gateway/peerload"
	"devshard/cmd/gateway/perf"
	"devshard/signing"
)

const (
	peerTestModel = "model-a"
	peerTestHost  = "gonka1host"
)

type refusingSigners struct{ asked *int }

func (r refusingSigners) SignerFor(string) (*signing.Secp256k1Signer, error) {
	*r.asked++
	return nil, errors.New("no key")
}

type countingRegistry struct{ reads *int }

func (c countingRegistry) MembersOf(context.Context, string, []string) ([]chain.RegistryMember, error) {
	*c.reads++
	return nil, nil
}

type fixedSigners struct{}

func (fixedSigners) SignerFor(string) (*signing.Secp256k1Signer, error) {
	return signing.SignerFromHex(strings.Repeat("11", 32))
}

// Test flow:
//  1. Build the channel from the default config, from a contract without the switch, and from the switch without a contract.
//  2. Assert each yields the nil plugin without asking for a key or reading the chain.
//  3. Assert the nil plugin's boot step starts nothing.
func TestPeerChannelIsOffUnlessSwitchedOnWithAContract(t *testing.T) {
	asked, reads := 0, 0
	sources := peerChannelSources{signers: refusingSigners{asked: &asked}, registry: countingRegistry{reads: &reads}}
	defaults := config.Defaults().Peer
	contractOnly := defaults
	contractOnly.RegistryContract = "gonka1contract"
	switchOnly := defaults
	switchOnly.Enabled = true

	for name, settings := range map[string]config.Peer{"defaults": defaults, "contract only": contractOnly, "switch only": switchOnly} {
		plugin, err := newPeerChannel(settings, sources)
		if err != nil || plugin != nil {
			t.Fatalf("%s: newPeerChannel() = %v, %v; want nil, nil", name, plugin, err)
		}
		if err := plugin.Start(context.Background()); err != nil {
			t.Fatalf("%s: Start() = %v", name, err)
		}
	}
	if asked != 0 || reads != 0 {
		t.Fatalf("switched off, but %d key reads and %d chain reads", asked, reads)
	}
}

// Test flow:
//  1. Switch the channel on without a chain connection, without a signing key, and with an invalid instance id.
//  2. Assert each refuses to build.
//  3. Build it complete from the signing key alone and assert it builds without reading the chain.
func TestPeerChannelRefusesAnIncompleteConfiguration(t *testing.T) {
	asked, reads := 0, 0
	active := config.Peer{Enabled: true, RegistryContract: "gonka1contract", InstanceID: "instance-a", Listen: ":0", AdvertiseURL: "https://a.example:8443", IdentityKeyEnv: "TEST_PEER_KEY"}

	if _, err := newPeerChannel(active, peerChannelSources{signers: fixedSigners{}}); !errors.Is(err, peerload.ErrBadSettings) {
		t.Fatalf("no chain: %v, want ErrBadSettings", err)
	}
	if _, err := newPeerChannel(active, peerChannelSources{signers: refusingSigners{asked: &asked}, registry: countingRegistry{reads: &reads}}); err == nil {
		t.Fatal("no signing key: want an error")
	}
	badInstance := active
	badInstance.InstanceID = "UPPER"
	if _, err := newPeerChannel(badInstance, peerChannelSources{signers: fixedSigners{}, registry: countingRegistry{reads: &reads}}); !errors.Is(err, peerload.ErrBadSettings) {
		t.Fatalf("bad instance id: %v, want ErrBadSettings", err)
	}
	if plugin, err := newPeerChannel(active, peerChannelSources{signers: fixedSigners{}, registry: countingRegistry{reads: &reads}}); err != nil || plugin == nil {
		t.Fatalf("complete settings: %v, %v", plugin, err)
	}
	if reads != 0 {
		t.Fatalf("building read the chain %d times", reads)
	}
}

// Test flow:
//  1. Join limiter windows and tracker hosts with one pair on each side and one on both.
//  2. Assert each pair carries the fields of the sides that know it, and the cut-off state maps to the wire.
func TestJoinLoadSamplesJoinsTheLimiterAndTheTracker(t *testing.T) {
	samples := joinLoadSamples(
		[]limits.HostWindow{
			{Participant: "both", Model: peerTestModel, InflightInputTokens: 5, InflightOutputTokens: 6, Cutoff: limits.CutoffOpen},
			{Participant: "windows", Model: peerTestModel, Cutoff: limits.CutoffHalfOpen},
		},
		[]perf.HostState{
			{Participant: "both", Model: peerTestModel, ModelInflight: 3, Ejected: true},
			{Participant: "hosts", Model: peerTestModel, ModelInflight: 4},
		},
		func(participant, _ string) perf.Pressure {
			if participant == "both" {
				return perf.Pressure{FirstContent: 1.5, Decode: 2.5}
			}
			return perf.Pressure{}
		},
	)
	byParticipant := map[string]peerload.LoadSample{}
	for _, sample := range samples {
		byParticipant[sample.Participant] = sample
	}

	wantBoth := peerload.LoadSample{Participant: "both", Model: peerTestModel, InflightRequests: 3, InflightInputTokens: 5, InflightOutputTokens: 6, Cutoff: peerload.CutoffOpen, Ejected: true, PressureFirstContent: 1.5, PressureDecode: 2.5}
	if byParticipant["both"] != wantBoth {
		t.Errorf("both = %+v, want %+v", byParticipant["both"], wantBoth)
	}
	if byParticipant["windows"].Cutoff != peerload.CutoffHalfOpen {
		t.Errorf("windows cutoff = %d, want half open", byParticipant["windows"].Cutoff)
	}
	if byParticipant["hosts"].InflightRequests != 4 || byParticipant["hosts"].Cutoff != peerload.CutoffClosed {
		t.Errorf("hosts = %+v", byParticipant["hosts"])
	}
}

// Test flow:
//  1. Listen on a loopback port and dial it through the channel's dialler with private addresses refused.
//  2. Assert the dial is refused and nothing was accepted.
//  3. Allow private addresses and assert the same dial connects.
func TestPeerDialsGoThroughTheAddressGuard(t *testing.T) {
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	t.Cleanup(func() { httpguard.SetAllowPrivate(false) })
	dial := peerDeps(config.Peer{}, nil, peerChannelSources{}).Dial

	httpguard.SetAllowPrivate(false)
	if conn, err := dial(context.Background(), "tcp", listener.Addr().String()); err == nil {
		_ = conn.Close()
		t.Fatalf("dialled %s with private addresses refused", listener.Addr())
	}

	httpguard.SetAllowPrivate(true)
	conn, err := dial(context.Background(), "tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("dial with private addresses allowed: %v", err)
	}
	_ = conn.Close()
}

// Test flow:
//  1. Build the channel's concurrency over a capacity holding generic weights only, then over one with by-model weights, then over one listing the model.
//  2. Store a peer's rows naming invented models in a view reading that concurrency.
//  3. Assert no invented model yields a series, and a model the chain names does.
func TestPeerCannotAddModelSeriesBeyondTheLocalModels(t *testing.T) {
	weights := map[string]float64{peerTestHost: 70}
	for name, snapshot := range map[string]chain.PhaseSnapshot{
		"generic weights only": {CurrentWeights: weights, FullWeights: weights},
		"by-model weights":     {CurrentWeights: weights, FullWeights: weights, CurrentWeightsByModel: map[string]map[string]float64{peerTestModel: weights}, FullWeightsByModel: map[string]map[string]float64{peerTestModel: weights}},
		"listed model":         {CurrentWeights: weights, FullWeights: weights, Models: map[string]chain.ModelParams{peerTestModel: {}}},
	} {
		t.Run(name, func(t *testing.T) {
			capacity := limits.NewCapacity(nil)
			capacity.Update(snapshot)
			holder := config.NewHolder(&config.Config{Limits: config.Limits{Concurrency: config.Concurrency{RequestsPer10000Weight: 5}}})
			now := time.UnixMilli(1_800_000_000_000)
			concurrency := peerDeps(config.Peer{}, nil, peerChannelSources{capacity: capacity, configHolder: holder}).Concurrency
			view := peerload.NewView(30*time.Second, concurrency, func() time.Time { return now })

			view.Store(peerload.PeerID{Address: "gonka1peer", Instance: "one"}, []peerload.HostLoad{
				{Participant: peerTestHost, Model: "invented", InflightRequests: 1},
				{Participant: peerTestHost, Model: peerTestModel, InflightRequests: 1},
			}, now.UnixMilli())

			rows := view.Rows()
			wantSeries := 1
			if name == "generic weights only" {
				wantSeries = 0
			}
			if len(rows) != wantSeries {
				t.Fatalf("Rows() = %+v, want %d series", rows, wantSeries)
			}
			for _, row := range rows {
				if row.Model != peerTestModel {
					t.Fatalf("series for a model nobody here serves: %+v", row)
				}
			}
		})
	}
}

// Test flow:
//  1. Track one host on two models and put one request in flight on the second model.
//  2. Join the samples the channel publishes.
//  3. Assert the request appears on its own model's row only.
func TestLocalSamplesCountInFlightRequestsPerModel(t *testing.T) {
	now := time.UnixMilli(1_800_000_000_000)
	tracker := perf.NewTracker(config.NewHolder(&config.Config{Perf: config.Perf{HostStalenessSeconds: 60, MinAvailableHosts: 1, MaxEjectionFraction: 0.5}}), func() time.Time { return now })
	tracker.RecordSample(perf.Sample{ParticipantKey: peerTestHost, Model: "model-a", Responsive: true})
	tracker.RecordSample(perf.Sample{ParticipantKey: peerTestHost, Model: "model-b", Responsive: true})
	tracker.Acquire(peerTestHost, "model-b")
	defer tracker.Release(peerTestHost, "model-b")

	samples := joinLoadSamples(nil, tracker.Snapshot(), tracker.Pressure)

	if len(samples) != 2 {
		t.Fatalf("samples = %+v, want two rows", samples)
	}
	for _, sample := range samples {
		want := int64(0)
		if sample.Model == "model-b" {
			want = 1
		}
		if sample.InflightRequests != want {
			t.Errorf("%s in flight = %d, want %d", sample.Model, sample.InflightRequests, want)
		}
	}
}

package peerload

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"devshard/cmd/gateway/chain"
	"devshard/cmd/gateway/internal/leakcheck"
)

const (
	firstAddress  = "gonka1first"
	secondAddress = "gonka1second"
	thirdAddress  = "gonka1third"
	testHeight    = int64(100_000)
	testHost      = "gonka1host"
	testModel     = "model-a"
	selfEndpoint  = "https://self.example:8443"
)

func TestMain(m *testing.M) {
	leakcheck.VerifyTestMain(m)
}

type fakeClock struct {
	mu      sync.Mutex
	current time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{current: time.UnixMilli(1_800_000_000_000)}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.current
}

func (c *fakeClock) advance(by time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.current = c.current.Add(by)
}

type fakeRegistry struct {
	mu         sync.Mutex
	members    map[string][]chain.RegistryInstance
	calls      [][]string
	failure    error
	refusal    error
	registered []chain.RegistryRegistration
}

func newFakeRegistry() *fakeRegistry {
	return &fakeRegistry{members: map[string][]chain.RegistryInstance{}}
}

func (r *fakeRegistry) MembersOf(_ context.Context, addresses []string) ([]chain.RegistryMember, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, addresses)
	if r.failure != nil {
		return nil, r.failure
	}
	members := make([]chain.RegistryMember, 0, len(addresses))
	for _, address := range addresses {
		members = append(members, chain.RegistryMember{Address: address, Instances: append([]chain.RegistryInstance(nil), r.members[address]...)})
	}
	return members, nil
}

func (r *fakeRegistry) put(address string, entry chain.RegistryInstance) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.putLocked(address, entry)
}

func (r *fakeRegistry) forgeEntry(address string, entry chain.RegistryInstance) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refusal = errors.New("registry refuses this write")
	r.putLocked(address, entry)
}

func (r *fakeRegistry) putLocked(address string, entry chain.RegistryInstance) {
	for index, existing := range r.members[address] {
		if existing.InstanceID == entry.InstanceID {
			r.members[address][index] = entry
			return
		}
	}
	r.members[address] = append(r.members[address], entry)
}

func (r *fakeRegistry) remove(address string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.members, address)
}

func (r *fakeRegistry) setFailure(failure error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failure = failure
}

func (r *fakeRegistry) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func (r *fakeRegistry) registrar(address string) Registrar {
	return registrarFunc(func(_ context.Context, registration chain.RegistryRegistration) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.refusal != nil {
			return r.refusal
		}
		r.registered = append(r.registered, registration)
		r.putLocked(address, chain.RegistryInstance{
			InstanceID:    registration.InstanceID,
			Endpoint:      registration.Endpoint,
			SPKISHA256:    registration.SPKISHA256,
			Protocol:      registration.Protocol,
			Topics:        registration.Topics,
			UpdatedHeight: uint64(testHeight),
		})
		return nil
	})
}

func (r *fakeRegistry) registrationCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.registered)
}

type registrarFunc func(context.Context, chain.RegistryRegistration) error

func (f registrarFunc) RegisterInstance(ctx context.Context, registration chain.RegistryRegistration) error {
	return f(ctx, registration)
}

func newChainKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, chainKeyBytes)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	return key
}

func newIdentity(t *testing.T, id PeerID) transportIdentity {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := issueIdentity(key, id)
	if err != nil {
		t.Fatal(err)
	}
	return identity
}

func registryEntry(instance string, identity transportIdentity, endpoint string) chain.RegistryInstance {
	return chain.RegistryInstance{
		InstanceID:    instance,
		Endpoint:      endpoint,
		SPKISHA256:    hex.EncodeToString(identity.Fingerprint[:]),
		Protocol:      protocolVersion,
		Topics:        []string{"load@1"},
		UpdatedHeight: uint64(testHeight),
	}
}

func stateOf(identity transportIdentity) tls.ConnectionState {
	return tls.ConnectionState{PeerCertificates: []*x509.Certificate{identity.Certificate.Leaf}, HandshakeComplete: true}
}

func refreshed(t *testing.T, members *membership) {
	t.Helper()
	if err := members.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh() = %v", err)
	}
}

func instanceNames(instances []peerInstance) []string {
	names := make([]string, 0, len(instances))
	for _, instance := range instances {
		names = append(names, instance.ID.String())
	}
	return names
}

func peerNames(ids []PeerID) []string {
	names := make([]string, 0, len(ids))
	for _, id := range ids {
		names = append(names, id.String())
	}
	return names
}

type recordingDialer struct {
	mu    sync.Mutex
	dials map[string]int
}

func newRecordingDialer() *recordingDialer {
	return &recordingDialer{dials: map[string]int{}}
}

func (d *recordingDialer) dial(ctx context.Context, network, address string) (net.Conn, error) {
	d.mu.Lock()
	d.dials[address]++
	d.mu.Unlock()
	var dialer net.Dialer
	return dialer.DialContext(ctx, network, address)
}

func (d *recordingDialer) count(address string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.dials[address]
}

type loopbackInstance struct {
	id       PeerID
	plugin   *Plugin
	listener net.Listener
	dialer   *recordingDialer
	address  string
	failures *failureLog
}

type failureLog struct {
	mu       sync.Mutex
	messages []error
}

func (l *failureLog) report(_ string, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.messages = append(l.messages, err)
}

func (l *failureLog) contains(target error) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, message := range l.messages {
		if errors.Is(message, target) {
			return true
		}
	}
	return false
}

func newLoopbackInstance(t *testing.T, id PeerID, registry *fakeRegistry, clock *fakeClock, samples func() []LoadSample, concurrency func(string, string) int64) *loopbackInstance {
	t.Helper()
	var config net.ListenConfig
	listener, err := config.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	instance := &loopbackInstance{id: id, listener: listener, dialer: newRecordingDialer(), address: listener.Addr().String(), failures: &failureLog{}}
	plugin, err := New(Settings{
		Self:          id,
		AdvertiseURL:  "https://" + instance.address,
		ListenAddress: instance.address,
		ChainKey:      newChainKey(t),
		Rounding:      DefaultRounding(),
	}, Deps{
		Registry:    registry,
		Registrar:   registry.registrar(id.Address),
		Samples:     samples,
		Concurrency: concurrency,
		Now:         clock.now,
		Listen: func(context.Context, string, string) (net.Listener, error) {
			return listener, nil
		},
		Dial:   instance.dialer.dial,
		Report: instance.failures.report,
	})
	if err != nil {
		t.Fatal(err)
	}
	instance.plugin = plugin
	return instance
}

func newTestPlugin(t *testing.T, registry Registry, registrar Registrar, now func() time.Time) *Plugin {
	t.Helper()
	plugin, err := New(Settings{Self: PeerID{Address: firstAddress, Instance: "self"}, AdvertiseURL: selfEndpoint, ChainKey: newChainKey(t), Rounding: DefaultRounding()}, Deps{
		Registry:    registry,
		Registrar:   registrar,
		Samples:     oneHostLoad(1),
		Concurrency: fixedConcurrency(1),
		Now:         now,
		Dial:        func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("no dial") },
		Report:      func(string, error) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	return plugin
}

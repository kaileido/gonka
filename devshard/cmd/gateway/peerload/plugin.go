package peerload

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"devshard/cmd/gateway/chain"
)

// Registrar writes this instance's own registry entry.
type Registrar interface {
	RegisterInstance(ctx context.Context, registration chain.RegistryRegistration) error
}

// Settings configures the channel. Shadow is the only mode: the discount reaches /metrics and nothing else.
type Settings struct {
	Self            PeerID
	AdvertiseURL    string
	ListenAddress   string
	TransportKeyPEM string
	Rounding        Rounding
	CommitPoll      time.Duration
}

// Deps is what the plugin reads and writes outside itself.
type Deps struct {
	Registry    Registry
	Registrar   Registrar
	Samples     func() []LoadSample
	Concurrency func(participant, model string) int64
	Now         func() time.Time
	Listen      func(ctx context.Context, network, address string) (net.Listener, error)
	Dial        DialFunc
	Report      func(message string, err error)
}

// Plugin is the whole channel behind one hook. A nil Plugin is the switched-off feature: it listens on nothing, dials nothing and starts no goroutine.
type Plugin struct {
	settings Settings
	deps     Deps
	identity transportIdentity
	members  *membership
	verifier *verifier
	view     *View
	topic    *loadTopic
	server   *peerServer
	poller   *poller

	nudge    chan struct{}
	finished sync.WaitGroup

	writeMu      sync.Mutex
	writeBackoff time.Duration
	nextWrite    time.Time
}

// New validates the settings and builds the plugin; it reads and dials nothing.
func New(settings Settings, deps Deps) (*Plugin, error) {
	if settings.Self.Address == emptyAddress || !validInstanceID(settings.Self.Instance) {
		return nil, fmt.Errorf("%w: instance id must be [a-z0-9-]+, at most %d bytes", ErrBadSettings, instanceIDLimit)
	}
	if !validEndpoint(settings.AdvertiseURL) {
		return nil, fmt.Errorf("%w: advertise URL must be https://host:port, at most %d bytes", ErrBadSettings, endpointLimit)
	}
	if deps.Registry == nil || deps.Registrar == nil {
		return nil, fmt.Errorf("%w: the channel needs a chain connection", ErrBadSettings)
	}
	identity, err := loadTransportIdentity(settings.TransportKeyPEM, settings.Self)
	if err != nil {
		return nil, err
	}
	members := newMembership(ownIdentity{ID: settings.Self, Fingerprint: identity.Fingerprint, AdvertiseURL: settings.AdvertiseURL}, deps.Registry, deps.Now)
	verifier := newVerifier(members)
	view := NewView(loadMaxAge, deps.Concurrency, deps.Now)
	topic := newLoadTopic(deps.Samples, settings.Rounding, view)
	return &Plugin{
		settings: settings,
		deps:     deps,
		identity: identity,
		members:  members,
		verifier: verifier,
		view:     view,
		topic:    topic,
		server:   newServer(settings.Self, verifier, identity, []peerTopic{topic}, deps.Now),
		poller:   newPoller(members, verifier, identity, view, deps.Dial, deps.Now, deps.Report),
		nudge:    make(chan struct{}, 1),
	}, nil
}

// ObserveChain takes one chain snapshot's allowlist and height; it never blocks the observer that calls it.
func (p *Plugin) ObserveChain(allowed []string, height int64) {
	if p == nil {
		return
	}
	if p.members.ObserveChain(allowed, height) {
		select {
		case p.nudge <- struct{}{}:
		default:
		}
	}
}

// Start opens the listener and the loops, which stop when ctx ends.
func (p *Plugin) Start(ctx context.Context) error {
	if p == nil {
		return nil
	}
	listener, err := p.deps.Listen(ctx, listenNetwork, p.settings.ListenAddress)
	if err != nil {
		return fmt.Errorf("peer listener %s: %w", p.settings.ListenAddress, err)
	}
	p.finished.Go(func() {
		if serveErr := p.server.Serve(listener); serveErr != nil {
			p.deps.Report("peer listener stopped", serveErr)
		}
	})
	p.finished.Go(func() { p.pollLoop(ctx) })
	p.finished.Go(func() { p.registryLoop(ctx) })
	p.finished.Go(func() {
		<-ctx.Done()
		_ = p.server.Close()
		p.poller.Close()
	})
	return nil
}

// Wait returns once a started plugin's loops have stopped.
func (p *Plugin) Wait() {
	if p != nil {
		p.finished.Wait()
	}
}

func (p *Plugin) refreshRegistry(ctx context.Context) {
	err := p.members.Refresh(ctx)
	if err != nil {
		p.deps.Report("peer registry read failed", err)
	}
	if err == nil || errors.Is(err, errPartialRead) {
		if err := p.announce(ctx); err != nil {
			p.deps.Report("peer registration failed", err)
		}
	}
	p.server.CloseRemoved()
	p.poller.Sync()
}

func (p *Plugin) pollPeers(ctx context.Context) { p.poller.Round(ctx, p.topic) }

// View is the shadow view of peer load.
func (p *Plugin) View() *View { return p.view }

// HandshakeCounts copies the handshake results so far, by result.
func (p *Plugin) HandshakeCounts() map[string]uint64 { return p.verifier.HandshakeCounts() }

func (p *Plugin) pollLoop(ctx context.Context) {
	ticker := time.NewTicker(p.topic.Interval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.pollPeers(ctx)
		}
	}
}

func (p *Plugin) registryLoop(ctx context.Context) {
	p.refreshRegistry(ctx)
	ticker := time.NewTicker(p.nextRegistryRead())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-p.nudge:
		}
		p.refreshRegistry(ctx)
		ticker.Reset(p.nextRegistryRead())
	}
}

func (p *Plugin) nextRegistryRead() time.Duration {
	if p.members.HasRead() {
		return registryRefreshInterval
	}
	return firstReadRetryInterval
}

func (p *Plugin) announce(ctx context.Context) error {
	self := p.settings.Self
	if !p.members.Allowed(self.Address) {
		return nil
	}
	if entry, found := p.members.Lookup(self); found && p.entryCurrent(entry) {
		return nil
	}
	if !p.writeDue() {
		return nil
	}
	writeCtx, cancel := context.WithTimeout(ctx, max(p.settings.CommitPoll, chain.DefaultPollTimeout)+registerBroadcastMargin)
	defer cancel()
	err := p.deps.Registrar.RegisterInstance(writeCtx, chain.RegistryRegistration{
		InstanceID: self.Instance,
		Endpoint:   p.settings.AdvertiseURL,
		SPKISHA256: hex.EncodeToString(p.identity.Fingerprint[:]),
		Protocol:   protocolVersion,
		Topics:     []string{topicLabel(p.topic)},
	})
	p.recordWrite(err)
	if err != nil {
		return fmt.Errorf("register instance %s: %w", self, err)
	}
	return p.members.Refresh(ctx)
}

func (p *Plugin) writeDue() bool {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	return !p.deps.Now().Before(p.nextWrite)
}

func (p *Plugin) recordWrite(err error) {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	if err == nil {
		p.writeBackoff = noWriteBackoff
		p.nextWrite = time.Time{}
		return
	}
	p.writeBackoff = min(max(p.writeBackoff*2, registryRefreshInterval), reregisterInterval)
	p.nextWrite = p.deps.Now().Add(p.writeBackoff)
}

func (p *Plugin) entryCurrent(entry peerInstance) bool {
	return entry.Endpoint == p.settings.AdvertiseURL &&
		entry.Fingerprint == p.identity.Fingerprint &&
		entry.SupportsTopic(p.topic.Name(), p.topic.Version()) &&
		p.members.Height()-int64(entry.UpdatedHeight) < reannounceBlocks
}

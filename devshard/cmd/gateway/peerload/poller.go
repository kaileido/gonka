package peerload

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"sync"
	"sync/atomic"
	"time"
)

// DialFunc opens the TCP connection under one peer's mTLS.
type DialFunc func(ctx context.Context, network, address string) (net.Conn, error)

type peerLink struct {
	instance  peerInstance
	client    *http.Client
	transport *http.Transport
	lastSeq   map[string]uint64

	handshakeUncounted atomic.Bool
}

type poller struct {
	members  *membership
	verifier *verifier
	identity transportIdentity
	view     *View
	dial     DialFunc
	now      func() time.Time
	report   func(string, error)

	mu    sync.Mutex
	links map[PeerID]*peerLink
}

func newPoller(members *membership, verifier *verifier, identity transportIdentity, view *View, dial DialFunc, now func() time.Time, report func(string, error)) *poller {
	return &poller{members: members, verifier: verifier, identity: identity, view: view, dial: dial, now: now, report: report, links: map[PeerID]*peerLink{}}
}

// Round polls every peer declaring the topic and returns once each has answered or failed; a failed peer's last snapshot ages out.
func (p *poller) Round(ctx context.Context, topic peerTopic) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.syncLocked()
	var polls sync.WaitGroup
	for _, link := range p.links {
		if !link.instance.SupportsTopic(topic.Name(), topic.Version()) {
			continue
		}
		polls.Go(func() {
			if err := p.poll(ctx, link, topic); err != nil {
				p.report("peer poll failed", err)
			}
		})
	}
	polls.Wait()
}

// Sync drops the links of peers that left or changed key or endpoint, and closes their connections; it waits for a round in progress.
func (p *poller) Sync() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.syncLocked()
}

// Close drops every link.
func (p *poller) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, link := range p.links {
		p.dropLocked(id, link)
	}
}

func (p *poller) syncLocked() {
	wanted := make(map[PeerID]peerInstance)
	for _, instance := range p.members.Peers() {
		wanted[instance.ID] = instance
	}
	for id, link := range p.links {
		instance, stays := wanted[id]
		if !stays || instance.Endpoint != link.instance.Endpoint || instance.Fingerprint != link.instance.Fingerprint || instance.Protocol != link.instance.Protocol {
			p.dropLocked(id, link)
		}
	}
	for id, instance := range wanted {
		if link, linked := p.links[id]; linked {
			link.instance = instance
			continue
		}
		p.links[id] = p.newLink(instance)
	}
}

func (p *poller) dropLocked(id PeerID, link *peerLink) {
	link.transport.CloseIdleConnections()
	delete(p.links, id)
	p.view.forget(id)
}

func (p *poller) newLink(instance peerInstance) *peerLink {
	protocols := new(http.Protocols)
	protocols.SetHTTP2(true)
	transport := &http.Transport{
		DialContext:     p.dial,
		TLSClientConfig: p.verifier.ClientTLSConfig(p.identity, instance.ID),
		Protocols:       protocols,

		MaxResponseHeaderBytes: maxHeaderBytes,
	}
	return &peerLink{
		instance:  instance,
		transport: transport,
		lastSeq:   map[string]uint64{},
		client: &http.Client{
			Transport:     transport,
			Timeout:       pollRequestTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

func (p *poller) poll(ctx context.Context, link *peerLink, topic peerTopic) error {
	traced := httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{TLSHandshakeDone: link.noteHandshake})
	request, err := http.NewRequestWithContext(traced, http.MethodGet, link.instance.Endpoint+peerPathPrefix+topic.Name(), http.NoBody)
	if err != nil {
		return fmt.Errorf("peer %s: %w", link.instance.ID, err)
	}
	response, err := link.client.Do(request)
	if err != nil {
		return fmt.Errorf("peer %s: %w", link.instance.ID, err)
	}
	defer func() { _ = response.Body.Close() }()
	if link.handshakeUncounted.Swap(false) {
		p.verifier.countAccepted()
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("peer %s: status %d", link.instance.ID, response.StatusCode)
	}
	body, err := readCapped(response.Body, topic.MaxBytes())
	if err != nil {
		return fmt.Errorf("peer %s: %w", link.instance.ID, err)
	}
	envelope, err := decodeEnvelope(body)
	if err != nil {
		return fmt.Errorf("peer %s: %w", link.instance.ID, err)
	}
	now := p.now()
	if err := checkEnvelope(envelope, topic, link.instance.ID, link.lastSeq[topic.Name()], now); err != nil {
		return fmt.Errorf("peer %s: %w", link.instance.ID, err)
	}
	link.lastSeq[topic.Name()] = envelope.Seq
	return topic.Accept(link.instance.ID, envelope, now)
}

func (l *peerLink) noteHandshake(_ tls.ConnectionState, err error) {
	if err == nil {
		l.handshakeUncounted.Store(true)
	}
}

func readCapped(body io.Reader, limit int) ([]byte, error) {
	read, err := io.ReadAll(io.LimitReader(body, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(read) > limit {
		return nil, errTooLarge
	}
	return read, nil
}

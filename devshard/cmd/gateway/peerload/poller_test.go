package peerload

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"

	"devshard/cmd/gateway/chain"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func cannedResponse(request *http.Request, status int, header http.Header, body []byte) *http.Response {
	return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(bytes.NewReader(body)), Request: request}
}

// Test flow:
//  1. Link to a peer that answers every poll with the same valid envelope.
//  2. Poll twice.
//  3. Assert the first is accepted and the second refused as a replay.
func TestPollRefusesAReplayedEnvelope(t *testing.T) {
	clock := newFakeClock()
	registry := newFakeRegistry()
	plugin := newTestPlugin(t, registry, registry.registrar(firstAddress), clock.now)
	id := PeerID{Address: secondAddress, Instance: "other"}
	encoded := peerEnvelope{Topic: loadTopicName, TopicVersion: loadTopicVersion, Origin: id.Address, Instance: id.Instance, Seq: 7, IssuedAtMS: clock.now().UnixMilli()}.marshal()
	link := plugin.poller.newLink(peerInstance{ID: id, Endpoint: "https://other.example:8443"})
	link.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return cannedResponse(request, http.StatusOK, http.Header{}, encoded), nil
	})

	if err := plugin.poller.poll(context.Background(), link, plugin.topic); err != nil {
		t.Fatalf("first poll = %v", err)
	}
	if err := plugin.poller.poll(context.Background(), link, plugin.topic); !errors.Is(err, errReplay) {
		t.Fatalf("second poll = %v, want errReplay", err)
	}
}

// Test flow:
//  1. Link to a peer that answers with a redirect.
//  2. Poll.
//  3. Assert one request was sent and the poll failed on the status.
func TestPollDoesNotFollowARedirect(t *testing.T) {
	registry := newFakeRegistry()
	plugin := newTestPlugin(t, registry, registry.registrar(firstAddress), newFakeClock().now)
	link := plugin.poller.newLink(peerInstance{ID: PeerID{Address: secondAddress, Instance: "other"}, Endpoint: "https://other.example:8443"})
	requests := 0
	link.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		return cannedResponse(request, http.StatusFound, http.Header{"Location": {"https://internal.example:8443/"}}, nil), nil
	})

	err := plugin.poller.poll(context.Background(), link, plugin.topic)

	if err == nil || !strings.Contains(err.Error(), "status 302") {
		t.Fatalf("poll = %v, want the 302 status", err)
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want 1", requests)
	}
}

// Test flow:
//  1. Register one peer declaring only another topic and one declaring the load topic at another version, and one declaring it at this version.
//  2. Run one poll round with a recording dialler.
//  3. Assert only the last was dialled.
func TestRoundPollsOnlyPeersDeclaringTheTopic(t *testing.T) {
	registry := newFakeRegistry()
	plugin := newTestPlugin(t, registry, registry.registrar(firstAddress), newFakeClock().now)
	entryWith := func(instance, endpoint string, topics ...string) chain.RegistryInstance {
		entry := registryEntry(instance, newIdentity(t, PeerID{Address: secondAddress, Instance: instance}), endpoint)
		entry.Topics = topics
		return entry
	}
	registry.put(secondAddress, entryWith("other-topic", "https://other-topic.example:8443", "other@1"))
	registry.put(secondAddress, entryWith("other-version", "https://other-version.example:8443", "load@2"))
	registry.put(secondAddress, entryWith("declares", "https://declares.example:8443", "load@1"))
	var mu sync.Mutex
	var dialled []string
	plugin.poller.dial = func(_ context.Context, _, address string) (net.Conn, error) {
		mu.Lock()
		defer mu.Unlock()
		dialled = append(dialled, address)
		return nil, errors.New("no dial")
	}
	plugin.ObserveChain([]string{secondAddress}, testHeight)
	plugin.refreshRegistry(context.Background())

	plugin.pollPeers(context.Background())

	if len(dialled) != 1 || dialled[0] != "declares.example:8443" {
		t.Fatalf("dialled %v, want only declares.example:8443", dialled)
	}
}

// Test flow:
//  1. Link to a peer and store a snapshot of it.
//  2. Change its key, its endpoint or its protocol on chain, and refresh.
//  3. Assert the link was replaced and the snapshot forgotten.
func TestChangedEntryReplacesTheLink(t *testing.T) {
	id := PeerID{Address: secondAddress, Instance: "other"}
	for name, change := range map[string]func(t *testing.T, entry *chain.RegistryInstance){
		"key": func(t *testing.T, entry *chain.RegistryInstance) {
			*entry = registryEntry(id.Instance, newIdentity(t, id), entry.Endpoint)
		},
		"endpoint": func(_ *testing.T, entry *chain.RegistryInstance) { entry.Endpoint = "https://moved.example:8443" },
		"protocol": func(_ *testing.T, entry *chain.RegistryInstance) { entry.Protocol = protocolVersion + 1 },
	} {
		t.Run(name, func(t *testing.T) {
			registry := newFakeRegistry()
			clock := newFakeClock()
			plugin := newTestPlugin(t, registry, registry.registrar(firstAddress), clock.now)
			entry := registryEntry(id.Instance, newIdentity(t, id), "https://other.example:8443")
			registry.put(secondAddress, entry)
			plugin.ObserveChain([]string{secondAddress}, testHeight)
			plugin.refreshRegistry(context.Background())
			link := plugin.poller.links[id]
			if link == nil {
				t.Fatal("no link after the first refresh")
			}
			plugin.view.Store(id, hostRow(1), clock.now().UnixMilli())

			change(t, &entry)
			registry.put(secondAddress, entry)
			plugin.refreshRegistry(context.Background())

			if plugin.poller.links[id] == link {
				t.Fatal("the link survived the changed entry")
			}
			if _, kept := plugin.view.LastSeenSeconds()[id]; kept {
				t.Fatal("the snapshot survived the changed entry")
			}
		})
	}
}

// Test flow:
//  1. Start a listener whose allowlist leaves out the dialling address, and link the dialling instance to it over real TLS 1.3.
//  2. Poll, so the dialling side finishes its handshake and the listener then refuses its certificate.
//  3. Assert neither side counts accepted, the listener counts not_allowed, the poll failure is reported, and no snapshot arrives.
func TestPollDoesNotCountAHandshakeTheListenerRefused(t *testing.T) {
	registry := newFakeRegistry()
	clock := newFakeClock()
	listening := newLoopbackInstance(t, PeerID{Address: secondAddress, Instance: "listening"}, registry, clock, oneHostLoad(1), fixedConcurrency(1))
	dialling := newLoopbackInstance(t, PeerID{Address: firstAddress, Instance: "dialling"}, registry, clock, oneHostLoad(1), fixedConcurrency(1))
	t.Cleanup(func() { _ = dialling.listener.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		listening.plugin.Wait()
	})
	allowlist := []string{secondAddress}
	listening.plugin.ObserveChain(allowlist, testHeight)
	if err := listening.plugin.Start(ctx); err != nil {
		t.Fatal(err)
	}
	listening.plugin.refreshRegistry(ctx)
	dialling.plugin.ObserveChain(allowlist, testHeight)
	dialling.plugin.refreshRegistry(ctx)

	dialling.plugin.pollPeers(ctx)

	if got := dialling.dialer.count(listening.address); got != 1 {
		t.Fatalf("dials = %d, want 1", got)
	}
	if got := dialling.plugin.HandshakeCounts()[HandshakeAccepted]; got != 0 {
		t.Errorf("dialling accepted = %d, want 0 for a refused certificate", got)
	}
	if got := listening.plugin.HandshakeCounts()[HandshakeAccepted]; got != 0 {
		t.Errorf("listening accepted = %d, want 0", got)
	}
	if got := listening.plugin.HandshakeCounts()[HandshakeNotAllowed]; got != 1 {
		t.Errorf("listening not_allowed = %d, want 1", got)
	}
	if len(dialling.failures.messages) != 1 {
		t.Errorf("reported failures = %v, want one", dialling.failures.messages)
	}
	if seen := dialling.plugin.View().LastSeenSeconds(); len(seen) != 0 {
		t.Errorf("last seen = %v, want none", seen)
	}
}

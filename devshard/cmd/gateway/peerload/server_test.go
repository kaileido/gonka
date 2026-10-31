package peerload

import (
	"cmp"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	floodSource = "192.0.2.1"
	otherSource = "192.0.2.2"
)

type countingTopic struct {
	*loadTopic
	builds  *atomic.Int64
	failure *atomic.Pointer[error]
}

func (c countingTopic) Build(time.Time) ([]byte, error) {
	if failure := c.failure.Load(); failure != nil {
		return nil, *failure
	}
	c.builds.Add(1)
	return []byte("payload"), nil
}

func newCountingServer(t *testing.T) (*peerServer, *verifierFixture, countingTopic) {
	t.Helper()
	fixture := newVerifierFixture(t)
	topic := countingTopic{
		loadTopic: newLoadTopic(func() []LoadSample { return nil }, DefaultRounding(), nil),
		builds:    &atomic.Int64{},
		failure:   &atomic.Pointer[error]{},
	}
	return newServer(fixture.members.self.ID, fixture.verifier, fixture.selfID, []peerTopic{topic}, fixture.clock.now), fixture, topic
}

func requestLoad(server *peerServer, identity transportIdentity) *httptest.ResponseRecorder {
	return requestTopic(server, identity, loadTopicName)
}

func requestTopic(server *peerServer, identity transportIdentity, topicName string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, peerPathPrefix+topicName, http.NoBody)
	state := stateOf(identity)
	request.TLS = &state
	return serve(server, request)
}

type recordingConn struct {
	net.Conn
	source string
	closed atomic.Bool
}

func (c *recordingConn) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP(c.source)}
}

func (c *recordingConn) Close() error {
	c.closed.Store(true)
	return nil
}

func connectOverPipe(t *testing.T, server *peerServer, identity transportIdentity) (*tls.Conn, *recordingConn) {
	t.Helper()
	serverSide, clientSide := net.Pipe()
	raw := &recordingConn{Conn: serverSide}
	connection := tls.Server(raw, server.http.TLSConfig)
	client := tls.Client(clientSide, &tls.Config{
		MinVersion: tls.VersionTLS13, InsecureSkipVerify: true, NextProtos: []string{http2Protocol}, //nolint:gosec
		Certificates: []tls.Certificate{identity.Certificate},
	})
	clientErr := make(chan error, 1)
	var draining sync.WaitGroup
	draining.Go(func() {
		clientErr <- client.HandshakeContext(t.Context())
		_, _ = io.Copy(io.Discard, client)
	})
	t.Cleanup(func() {
		_ = serverSide.Close()
		_ = clientSide.Close()
		draining.Wait()
	})
	if err := connection.HandshakeContext(t.Context()); err != nil {
		t.Fatalf("server HandshakeContext() = %v, want nil", err)
	}
	if err := <-clientErr; err != nil {
		t.Fatalf("client HandshakeContext() = %v, want nil", err)
	}
	server.trackConn(connection, http.StateNew)
	return connection, raw
}

// Test flow:
//  1. Spend one peer's whole burst at one instant and assert each request answers 200.
//  2. Assert the next one answers 429 with the rate_limited header and no body.
//  3. Assert another instance of the same address still has its own allowance.
//  4. Advance the clock one poll interval and assert the peer is served again, once.
func TestAPeerOverItsAllowanceAnswers429AndRecovers(t *testing.T) {
	server, fixture, _ := newCountingServer(t)
	sibling := PeerID{Address: secondAddress, Instance: "sibling"}
	siblingIdentity := newIdentity(t, sibling)
	fixture.registry.put(secondAddress, registryEntry(sibling.Instance, siblingIdentity, "https://sibling.example:8443"))
	refreshed(t, fixture.members)

	for range requestBurstPerTopic {
		if got := requestLoad(server, fixture.peer).Code; got != http.StatusOK {
			t.Fatalf("requestLoad(peer) = %d inside the burst, want %d", got, http.StatusOK)
		}
	}
	limited := requestLoad(server, fixture.peer)
	if limited.Code != http.StatusTooManyRequests || limited.Header().Get(headerPeerError) != peerErrorRateLimited || limited.Body.Len() != 0 {
		t.Fatalf("requestLoad(peer) = %d, header %q, %d body bytes past the burst, want %d, %q, 0", limited.Code, limited.Header().Get(headerPeerError), limited.Body.Len(), http.StatusTooManyRequests, peerErrorRateLimited)
	}
	if got := requestLoad(server, siblingIdentity).Code; got != http.StatusOK {
		t.Fatalf("requestLoad(sibling) = %d, want %d", got, http.StatusOK)
	}

	fixture.clock.advance(loadInterval)

	if got := requestLoad(server, fixture.peer).Code; got != http.StatusOK {
		t.Fatalf("requestLoad(peer) = %d after one interval, want %d", got, http.StatusOK)
	}
	if got := requestLoad(server, fixture.peer).Code; got != http.StatusTooManyRequests {
		t.Fatalf("requestLoad(peer) = %d on the second request after one interval, want %d", got, http.StatusTooManyRequests)
	}
}

// Test flow:
//  1. Poll once every interval for many intervals.
//  2. Assert no poll is ever limited.
func TestPollingAtTheIntervalIsNeverLimited(t *testing.T) {
	const polls = 100
	server, fixture, _ := newCountingServer(t)

	for poll := range polls {
		if got := requestLoad(server, fixture.peer).Code; got != http.StatusOK {
			t.Fatalf("requestLoad(peer) = %d on poll %d, want %d", got, poll, http.StatusOK)
		}
		fixture.clock.advance(loadInterval)
	}
}

// Test flow:
//  1. Spend the whole burst of a peer on one topic and assert the next request answers 429.
//  2. Assert the same peer's request for another topic is not limited.
//  3. Request an unknown topic past the burst and assert each answers 404 and holds no allowance.
func TestAPeersAllowanceIsPerTopicAndAnUnknownTopicSpendsNone(t *testing.T) {
	server, fixture := newServerFixture(t)
	for range requestBurstPerTopic {
		requestLoad(server, fixture.peer)
	}
	if got := requestLoad(server, fixture.peer).Code; got != http.StatusTooManyRequests {
		t.Fatalf("requestLoad(peer) = %d past the burst, want %d", got, http.StatusTooManyRequests)
	}

	if got := requestTopic(server, fixture.peer, "oversized").Code; got == http.StatusTooManyRequests {
		t.Fatalf("requestTopic(peer, oversized) = %d, want it not limited", got)
	}

	held := len(server.limiters)
	for range requestBurstPerTopic + 1 {
		if got := requestTopic(server, fixture.peer, "absent").Code; got != http.StatusNotFound {
			t.Fatalf("requestTopic(peer, absent) = %d, want %d", got, http.StatusNotFound)
		}
	}
	if got := len(server.limiters); got != held {
		t.Fatalf("len(limiters) = %d after unknown topics, want %d", got, held)
	}
}

// Test flow:
//  1. Spend the whole burst of a peer and of a listed peer.
//  2. Remove the first peer from the registry, run the close of removed peers, and add it back.
//  3. Assert the returning peer has a fresh allowance and the listed peer stays limited.
func TestAllowanceOfARemovedPeerIsForgotten(t *testing.T) {
	server, fixture, _ := newCountingServer(t)
	listed := PeerID{Address: firstAddress, Instance: "listed"}
	listedIdentity := newIdentity(t, listed)
	fixture.registry.put(firstAddress, registryEntry(listed.Instance, listedIdentity, "https://listed.example:8443"))
	refreshed(t, fixture.members)
	for range requestBurstPerTopic {
		requestLoad(server, fixture.peer)
		requestLoad(server, listedIdentity)
	}

	fixture.registry.remove(secondAddress)
	refreshed(t, fixture.members)
	server.CloseRemoved()
	fixture.registry.put(secondAddress, registryEntry(fixture.peerID.Instance, fixture.peer, "https://returning.example:8443"))
	refreshed(t, fixture.members)

	if got := requestLoad(server, fixture.peer).Code; got != http.StatusOK {
		t.Fatalf("requestLoad(returning peer) = %d, want %d", got, http.StatusOK)
	}
	if got := requestLoad(server, listedIdentity).Code; got != http.StatusTooManyRequests {
		t.Fatalf("requestLoad(listed peer) = %d, want %d", got, http.StatusTooManyRequests)
	}
}

// Test flow:
//  1. Send many concurrent requests inside one cache window.
//  2. Assert one build, and that every response decodes to the same payload.
//  3. Walk the responses in sequence order through the receiver's check with its last sequence, late in the window, and assert each passes.
//  4. Assert the second window builds again and its first response follows the last sequence.
func TestConcurrentRequestsShareOneBuildAndEachPassesTheReceiversCheck(t *testing.T) {
	const (
		concurrent            = 32
		requestsAfterTheBurst = 2
	)
	server, fixture, topic := newCountingServer(t)
	server.requestBurst = concurrent + requestsAfterTheBurst
	builtAt := fixture.clock.now()

	responses := make([]*httptest.ResponseRecorder, concurrent)
	var requests sync.WaitGroup
	for index := range responses {
		requests.Go(func() { responses[index] = requestLoad(server, fixture.peer) })
	}
	requests.Wait()

	if got := topic.builds.Load(); got != 1 {
		t.Fatalf("builds = %d, want 1", got)
	}
	envelopes := make([]peerEnvelope, 0, concurrent)
	for _, response := range responses {
		envelope, err := decodeEnvelope(response.Body.Bytes())
		if err != nil {
			t.Fatal(err)
		}
		if string(envelope.Payload) != "payload" || envelope.IssuedAtMS != builtAt.UnixMilli() {
			t.Fatalf("decodeEnvelope() = payload %q, issued %d, want %q, %d", envelope.Payload, envelope.IssuedAtMS, "payload", builtAt.UnixMilli())
		}
		envelopes = append(envelopes, envelope)
	}
	slices.SortFunc(envelopes, func(first, second peerEnvelope) int { return cmp.Compare(first.Seq, second.Seq) })
	fixture.clock.advance(payloadCacheWindow - time.Millisecond)
	selfID := fixture.members.self.ID
	receiverNow := fixture.clock.now()
	var lastSeq uint64
	for _, envelope := range envelopes {
		if err := checkEnvelope(envelope, topic, selfID, lastSeq, receiverNow); err != nil {
			t.Fatal(err)
		}
		lastSeq = envelope.Seq
	}

	insideWindow, err := decodeEnvelope(requestLoad(server, fixture.peer).Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if err := checkEnvelope(insideWindow, topic, selfID, lastSeq, receiverNow); err != nil {
		t.Fatalf("checkEnvelope(repeated poll inside the window) = %v, want nil", err)
	}
	if got := topic.builds.Load(); got != 1 {
		t.Fatalf("builds = %d inside the window, want 1", got)
	}

	fixture.clock.advance(time.Millisecond)
	nextWindow, err := decodeEnvelope(requestLoad(server, fixture.peer).Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if err := checkEnvelope(nextWindow, topic, selfID, insideWindow.Seq, fixture.clock.now()); err != nil {
		t.Fatal(err)
	}
	if got := topic.builds.Load(); got != 2 {
		t.Fatalf("builds = %d in the next window, want 2", got)
	}
}

// Test flow:
//  1. Make the topic's build fail and assert the request answers 500.
//  2. Clear the failure inside the same window and assert the next request builds and answers 200.
func TestAFailedBuildIsNotCached(t *testing.T) {
	server, fixture, topic := newCountingServer(t)
	failure := errors.New("no load")
	topic.failure.Store(&failure)
	if got := requestLoad(server, fixture.peer).Code; got != http.StatusInternalServerError {
		t.Fatalf("requestLoad(peer) = %d on a failing build, want %d", got, http.StatusInternalServerError)
	}

	topic.failure.Store(nil)

	if got := requestLoad(server, fixture.peer).Code; got != http.StatusOK {
		t.Fatalf("requestLoad(peer) = %d after the failure, want %d", got, http.StatusOK)
	}
}

// Test flow:
//  1. Admit connections up to the total cap and assert none is closed.
//  2. Offer one more and assert it is closed at accept.
//  3. Close one admitted connection and assert a new one takes its place.
func TestConnectionsPastTheTotalCapAreClosedAtAccept(t *testing.T) {
	server, _ := newServerFixture(t)
	server.maxConnections = 2
	first, second, excess, replacement := &recordingConn{}, &recordingConn{}, &recordingConn{}, &recordingConn{}

	server.trackConn(first, http.StateNew)
	server.trackConn(second, http.StateNew)
	if first.closed.Load() || second.closed.Load() {
		t.Fatal("trackConn(StateNew) closed a connection inside the cap, want it open")
	}
	server.trackConn(excess, http.StateNew)
	if !excess.closed.Load() {
		t.Fatal("trackConn(StateNew) left the connection past the cap open, want it closed")
	}
	server.trackConn(excess, http.StateClosed)
	server.trackConn(first, http.StateClosed)
	server.trackConn(replacement, http.StateNew)
	if replacement.closed.Load() {
		t.Fatal("trackConn(StateNew) closed the connection after a close, want it open")
	}
}

// Test flow:
//  1. Offer a TLS connection that has not finished its handshake.
//  2. Run the close of removed peers.
//  3. Assert the connection stays open: it has no peer to check yet.
func TestAConnectionInItsHandshakeSurvivesTheCloseOfRemovedPeers(t *testing.T) {
	server, _ := newServerFixture(t)
	raw := &recordingConn{}
	pending := tls.Server(raw, &tls.Config{MinVersion: tls.VersionTLS13})
	server.trackConn(pending, http.StateNew)

	server.CloseRemoved()

	if raw.closed.Load() {
		t.Fatal("CloseRemoved() closed a connection still in its handshake, want it open")
	}
}

// Test flow:
//  1. Set the total cap two above the limit on unidentified connections per source.
//  2. Offer the limit from one source and assert all stay open, then one more and assert it is closed at accept.
//  3. Offer one from another source and assert it stays open, so the first source did not fill the listener.
//  4. Identify one connection of the first source and assert that source can connect again.
func TestOneSourceCannotFillTheListenerWithUnidentifiedConnections(t *testing.T) {
	server, _ := newServerFixture(t)
	server.maxConnections = maxUnidentifiedPerSource + 2
	flood := make([]*recordingConn, maxUnidentifiedPerSource)
	for index := range flood {
		flood[index] = &recordingConn{source: floodSource}
		server.trackConn(flood[index], http.StateNew)
		if flood[index].closed.Load() {
			t.Fatalf("trackConn(StateNew) closed connection %d of the source, want it open", index)
		}
	}
	excess := &recordingConn{source: floodSource}
	server.trackConn(excess, http.StateNew)
	if !excess.closed.Load() {
		t.Fatal("trackConn(StateNew) left the connection past the source limit open, want it closed")
	}
	server.trackConn(excess, http.StateClosed)

	newcomer := &recordingConn{source: otherSource}
	server.trackConn(newcomer, http.StateNew)
	if newcomer.closed.Load() {
		t.Fatal("trackConn(StateNew) closed another source's connection, want it open")
	}

	server.trackConn(flood[0], http.StateActive)
	identifiedSource := &recordingConn{source: floodSource}
	server.trackConn(identifiedSource, http.StateNew)
	if identifiedSource.closed.Load() {
		t.Fatal("trackConn(StateNew) closed a connection after the source's first was identified, want it open")
	}
}

// Test flow:
//  1. Serve with a per-address cap of one and complete a handshake over a pipe for two instances of one address and one of another.
//  2. Identify the first instance and assert it stays open and counts once.
//  3. Identify its sibling and assert it is closed, its address still counts one before the close completes, and it is not counted as accepted.
//  4. Identify the instance of the other address and assert the cap did not reach it.
//  5. Finish the sibling's close and assert the first still counts, then finish the first and assert the count is empty.
func TestConnectionsPastTheAddressCapAreClosedOnceIdentified(t *testing.T) {
	server, fixture := newServerFixture(t)
	server.maxConnectionsPerAddress = 1
	sibling := PeerID{Address: secondAddress, Instance: "sibling"}
	other := PeerID{Address: thirdAddress, Instance: "other"}
	siblingIdentity, otherIdentity := newIdentity(t, sibling), newIdentity(t, other)
	fixture.registry.put(sibling.Address, registryEntry(sibling.Instance, siblingIdentity, "https://sibling.example:8443"))
	fixture.registry.put(other.Address, registryEntry(other.Instance, otherIdentity, "https://other.example:8443"))
	fixture.members.ObserveChain([]string{firstAddress, secondAddress, thirdAddress}, testHeight)
	refreshed(t, fixture.members)
	first, firstRaw := connectOverPipe(t, server, fixture.peer)
	second, secondRaw := connectOverPipe(t, server, siblingIdentity)
	third, thirdRaw := connectOverPipe(t, server, otherIdentity)

	server.trackConn(first, http.StateActive)
	if firstRaw.closed.Load() {
		t.Fatal("trackConn(first, StateActive) closed it, want it open")
	}
	if got := server.connectionsOf(secondAddress); got != 1 {
		t.Fatalf("connectionsOf(capped address) = %d after the first, want 1", got)
	}

	server.trackConn(second, http.StateActive)
	if !secondRaw.closed.Load() {
		t.Fatal("trackConn(sibling, StateActive) left it open, want it closed")
	}
	if got := server.connectionsOf(secondAddress); got != 1 {
		t.Fatalf("connectionsOf(capped address) = %d while the sibling closes, want 1", got)
	}
	if got := server.verifier.HandshakeCounts()[HandshakeAccepted]; got != 1 {
		t.Fatalf("HandshakeCounts()[accepted] = %d after the refusal, want 1", got)
	}

	server.trackConn(third, http.StateActive)
	if thirdRaw.closed.Load() {
		t.Fatal("trackConn(other address, StateActive) closed it, want it open")
	}

	server.trackConn(second, http.StateClosed)
	if got := server.connectionsOf(secondAddress); got != 1 {
		t.Fatalf("connectionsOf(capped address) = %d after the sibling closed, want 1", got)
	}
	server.trackConn(first, http.StateClosed)
	if got := server.connectionsOf(secondAddress); got != 0 {
		t.Fatalf("connectionsOf(capped address) = %d after the first closed, want 0", got)
	}
}

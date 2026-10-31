package peerload

import (
	"crypto/tls"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

type peerServer struct {
	self                     PeerID
	verifier                 *verifier
	payloads                 map[string]*payloadCache
	now                      func() time.Time
	http                     *http.Server
	requestBurst             int
	maxConnections           int
	maxConnectionsPerAddress int

	mu                  sync.Mutex
	connectionSources   map[net.Conn]string
	connectionAddresses map[net.Conn]string
	limiters            map[topicAllowance]*rate.Limiter
	lastSeq             uint64
}

type topicAllowance struct {
	peer  PeerID
	topic string
}

type payloadCache struct {
	topic peerTopic

	mu      sync.Mutex
	filled  bool
	builtAt time.Time
	payload []byte
}

func newServer(self PeerID, verifier *verifier, identity transportIdentity, topics []peerTopic, now func() time.Time) *peerServer {
	payloads := make(map[string]*payloadCache, len(topics))
	for _, topic := range topics {
		payloads[topic.Name()] = &payloadCache{topic: topic}
	}
	server := &peerServer{
		self:                     self,
		verifier:                 verifier,
		payloads:                 payloads,
		now:                      now,
		requestBurst:             requestBurstPerTopic,
		maxConnections:           maxConnections,
		maxConnectionsPerAddress: maxConnectionsPerAddress,
		connectionSources:        map[net.Conn]string{},
		connectionAddresses:      map[net.Conn]string{},
		limiters:                 map[topicAllowance]*rate.Limiter{},
	}
	protocols := new(http.Protocols)
	protocols.SetHTTP2(true)
	server.http = &http.Server{
		Handler:           server.Handler(),
		TLSConfig:         verifier.ServerTLSConfig(identity),
		ReadHeaderTimeout: serverHeaderTimeout,
		IdleTimeout:       serverIdleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		Protocols:         protocols,
		ConnState:         server.trackConn,
	}
	return server
}

// Handler is the request path alone; a request not on a verified connection is refused.
func (s *peerServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(peerRoute, s.serveTopic)
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		id, err := s.verifier.Authorize(request.TLS)
		if err != nil {
			refuse(writer, http.StatusForbidden, peerErrorRefused)
			return
		}
		if !s.allowRequest(id, strings.TrimPrefix(request.URL.Path, peerPathPrefix)) {
			refuse(writer, http.StatusTooManyRequests, peerErrorRateLimited)
			return
		}
		mux.ServeHTTP(writer, request)
	})
}

// Serve blocks until Close; the error is nil after a Close.
func (s *peerServer) Serve(listener net.Listener) error {
	if err := s.http.ServeTLS(listener, "", ""); !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("peer listener: %w", err)
	}
	return nil
}

// Close stops the listener and every connection.
func (s *peerServer) Close() error { return s.http.Close() }

// CloseRemoved closes every connection whose peer no longer passes the check and forgets the request allowance of a peer that left; a connection still in its handshake has no peer to check yet.
func (s *peerServer) CloseRemoved() {
	s.mu.Lock()
	conns := slices.Collect(maps.Keys(s.connectionAddresses))
	for allowance := range s.limiters {
		if !s.verifier.Listed(allowance.peer) {
			delete(s.limiters, allowance)
		}
	}
	s.mu.Unlock()
	for _, conn := range conns {
		if _, err := s.authorizeConnection(conn); err != nil {
			_ = conn.Close()
		}
	}
}

func (s *peerServer) trackConn(conn net.Conn, state http.ConnState) {
	switch state {
	case http.StateNew:
		if !s.admit(conn) {
			_ = conn.Close()
		}
	case http.StateActive, http.StateIdle:
		if !s.identify(conn) {
			_ = conn.Close()
		}
	case http.StateClosed, http.StateHijacked:
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.connectionSources, conn)
		delete(s.connectionAddresses, conn)
	}
}

func (s *peerServer) admit(conn net.Conn) bool {
	source := sourceOf(conn)
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.connectionSources) >= s.maxConnections || s.unidentifiedFrom(source) >= maxUnidentifiedPerSource {
		return false
	}
	s.connectionSources[conn] = source
	return true
}

func (s *peerServer) identify(conn net.Conn) bool {
	s.mu.Lock()
	_, known := s.connectionAddresses[conn]
	s.mu.Unlock()
	if known {
		return true
	}
	address := s.peerAddress(conn)
	s.mu.Lock()
	defer s.mu.Unlock()
	if address != emptyAddress && s.connectionsOf(address) >= s.maxConnectionsPerAddress {
		s.connectionAddresses[conn] = emptyAddress
		return false
	}
	s.connectionAddresses[conn] = address
	s.verifier.countAccepted()
	return true
}

func (s *peerServer) peerAddress(conn net.Conn) string {
	id, err := s.authorizeConnection(conn)
	if err != nil {
		return emptyAddress
	}
	return id.Address
}

func (s *peerServer) authorizeConnection(conn net.Conn) (PeerID, error) {
	tlsConn, isTLS := conn.(*tls.Conn)
	if !isTLS {
		return PeerID{}, errIdentity
	}
	state := tlsConn.ConnectionState()
	return s.verifier.Authorize(&state)
}

func (s *peerServer) connectionsOf(address string) int {
	count := 0
	for _, connected := range s.connectionAddresses {
		if connected == address {
			count++
		}
	}
	return count
}

func (s *peerServer) unidentifiedFrom(source string) int {
	count := 0
	for conn, connected := range s.connectionSources {
		if _, identified := s.connectionAddresses[conn]; !identified && connected == source {
			count++
		}
	}
	return count
}

func (s *peerServer) allowRequest(id PeerID, topicName string) bool {
	cache, known := s.payloads[topicName]
	if !known {
		return true
	}
	allowance := topicAllowance{peer: id, topic: topicName}
	s.mu.Lock()
	defer s.mu.Unlock()
	limiter, held := s.limiters[allowance]
	if !held {
		limiter = rate.NewLimiter(rate.Every(cache.topic.Interval()), s.requestBurst)
		s.limiters[allowance] = limiter
	}
	return limiter.AllowN(s.now(), 1)
}

func (s *peerServer) serveTopic(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writer.Header().Set(headerAllow, http.MethodGet)
		writer.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if request.ContentLength != 0 {
		refuse(writer, http.StatusBadRequest, peerErrorBodyForbidden)
		return
	}
	cache, known := s.payloads[request.PathValue(topicPathValue)]
	if !known {
		refuse(writer, http.StatusNotFound, peerErrorUnknownTopic)
		return
	}
	topic := cache.topic
	now := s.now()
	payload, builtAt, err := cache.read(now)
	if err != nil {
		writer.WriteHeader(http.StatusInternalServerError)
		return
	}
	encoded := peerEnvelope{
		Topic:        topic.Name(),
		TopicVersion: topic.Version(),
		Origin:       s.self.Address,
		Instance:     s.self.Instance,
		Seq:          s.nextSeq(now),
		IssuedAtMS:   builtAt.UnixMilli(),
		Payload:      payload,
	}.marshal()
	if len(encoded) > topic.MaxBytes() {
		writer.WriteHeader(http.StatusInternalServerError)
		return
	}
	writer.Header().Set(headerContentType, contentType)
	_, _ = writer.Write(encoded)
}

func (s *peerServer) nextSeq(now time.Time) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastSeq = max(uint64(now.UnixNano()), s.lastSeq+1)
	return s.lastSeq
}

func (c *payloadCache) read(now time.Time) ([]byte, time.Time, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.filled && now.Sub(c.builtAt) < payloadCacheWindow {
		return c.payload, c.builtAt, nil
	}
	payload, err := c.topic.Build(now)
	if err != nil {
		return nil, time.Time{}, err
	}
	c.filled, c.builtAt, c.payload = true, now, payload
	return payload, now, nil
}

func refuse(writer http.ResponseWriter, status int, reason string) {
	writer.Header().Set(headerPeerError, reason)
	writer.WriteHeader(status)
}

func sourceOf(conn net.Conn) string {
	remote := conn.RemoteAddr().String()
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		return remote
	}
	return host
}

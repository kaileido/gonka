package peerload

import (
	"crypto/tls"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"slices"
	"sync"
	"time"
)

type peerServer struct {
	self     PeerID
	verifier *verifier
	topics   map[string]peerTopic
	now      func() time.Time
	http     *http.Server

	mu      sync.Mutex
	conns   map[net.Conn]struct{}
	lastSeq uint64
}

func newServer(self PeerID, verifier *verifier, identity transportIdentity, topics []peerTopic, now func() time.Time) *peerServer {
	byName := make(map[string]peerTopic, len(topics))
	for _, topic := range topics {
		byName[topic.Name()] = topic
	}
	server := &peerServer{self: self, verifier: verifier, topics: byName, now: now, conns: map[net.Conn]struct{}{}}
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
		if _, err := s.verifier.Authorize(request.TLS); err != nil {
			refuse(writer, http.StatusForbidden, peerErrorRefused)
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

// CloseRemoved closes every connection whose peer no longer passes the check.
func (s *peerServer) CloseRemoved() {
	s.mu.Lock()
	conns := slices.Collect(maps.Keys(s.conns))
	s.mu.Unlock()
	for _, conn := range conns {
		tlsConn, isTLS := conn.(*tls.Conn)
		if !isTLS {
			continue
		}
		state := tlsConn.ConnectionState()
		if _, err := s.verifier.Authorize(&state); err != nil {
			_ = conn.Close()
		}
	}
}

func (s *peerServer) trackConn(conn net.Conn, state http.ConnState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch state {
	case http.StateActive, http.StateIdle:
		if _, tracked := s.conns[conn]; !tracked {
			s.verifier.countAccepted()
		}
		s.conns[conn] = struct{}{}
	case http.StateClosed, http.StateHijacked:
		delete(s.conns, conn)
	}
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
	topic, known := s.topics[request.PathValue(topicPathValue)]
	if !known {
		refuse(writer, http.StatusNotFound, peerErrorUnknownTopic)
		return
	}
	now := s.now()
	payload, err := topic.Build(now)
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
		IssuedAtMS:   now.UnixMilli(),
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

func refuse(writer http.ResponseWriter, status int, reason string) {
	writer.Header().Set(headerPeerError, reason)
	writer.WriteHeader(status)
}

package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

var (
	errHTTPConnClosed      = errors.New("http connection is closed")
	errHTTPWriteDeadline   = errors.New("http write deadline exceeded")
	errHTTPPendingLimit    = errors.New("http pending message limit exceeded")
	errHTTPMessageTooLarge = errors.New("http message too large")
	errHTTPPollActive      = errors.New("http poll already active")
	errHTTPSequenceInvalid = errors.New("invalid HTTP message sequence")
)

type httpRelayConfig struct {
	pollWait         time.Duration
	sessionTTL       time.Duration
	maxSessions      int
	maxSessionsPerIP int
	maxMessageBytes  int
	maxBatchMessages int
	maxPendingBytes  int
	streamKeepalive  time.Duration
}

func defaultHTTPRelayConfig() httpRelayConfig {
	return httpRelayConfig{
		pollWait:         25 * time.Second,
		sessionTTL:       120 * time.Second,
		maxSessions:      1000,
		maxSessionsPerIP: 20,
		maxMessageBytes:  1 << 20,
		maxBatchMessages: 32,
		maxPendingBytes:  4 << 20,
		streamKeepalive:  15 * time.Second,
	}
}

type sequencedMessage struct {
	sequence uint64
	payload  []byte
}

type httpSequenceError struct {
	expected  uint64
	received  uint64
	duplicate bool
}

func (e *httpSequenceError) Error() string {
	if e.duplicate {
		return fmt.Sprintf("duplicate HTTP message sequence %d", e.received)
	}
	return fmt.Sprintf("expected HTTP message sequence %d, received %d", e.expected, e.received)
}

type httpRelayManager struct {
	mu          sync.Mutex
	sessions    map[string]*httpSession
	config      httpRelayConfig
	sweepCancel context.CancelFunc
	sweepOnce   sync.Once
}

func newHTTPRelayManager(config httpRelayConfig) *httpRelayManager {
	return &httpRelayManager{
		sessions: make(map[string]*httpSession),
		config:   config,
	}
}

func (m *httpRelayManager) create(remoteAddr net.Addr) (*httpSession, string, error) {
	m.expireInactive()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.config.maxSessions > 0 && len(m.sessions) >= m.config.maxSessions {
		return nil, "", errors.New("HTTP relay session limit reached")
	}
	if m.config.maxSessionsPerIP > 0 {
		ip := getIPFromAddr(remoteAddr)
		count := 0
		for _, session := range m.sessions {
			if getIPFromAddr(session.remoteAddr) == ip {
				count++
			}
		}
		if count >= m.config.maxSessionsPerIP {
			return nil, "", errors.New("HTTP relay per-IP session limit reached")
		}
	}

	for attempt := 0; attempt < 3; attempt++ {
		idBytes := make([]byte, 16)
		if _, err := rand.Read(idBytes); err != nil {
			return nil, "", err
		}
		tokenBytes := make([]byte, 32)
		if _, err := rand.Read(tokenBytes); err != nil {
			return nil, "", err
		}
		id := base64.RawURLEncoding.EncodeToString(idBytes)
		if _, exists := m.sessions[id]; exists {
			continue
		}
		session := newHTTPSession(id, sha256.Sum256(tokenBytes), remoteAddr, m)
		m.sessions[id] = session
		return session, base64.RawURLEncoding.EncodeToString(tokenBytes), nil
	}
	return nil, "", errors.New("unable to allocate an HTTP relay session identifier")
}

func (m *httpRelayManager) get(id string) *httpSession {
	m.expireInactive()
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[id]
}

func (m *httpRelayManager) start() {
	m.sweepOnce.Do(func() {
		interval := m.config.sessionTTL / 2
		if interval <= 0 {
			interval = time.Second
		}
		ctx, cancel := context.WithCancel(context.Background())
		m.mu.Lock()
		m.sweepCancel = cancel
		m.mu.Unlock()
		go func() {
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					m.expireInactive()
				case <-ctx.Done():
					return
				}
			}
		}()
	})
}

func (m *httpRelayManager) expireInactive() {
	ttl := m.config.sessionTTL
	if ttl <= 0 {
		return
	}
	cutoff := time.Now().Add(-ttl)
	m.mu.Lock()
	sessions := make([]*httpSession, 0, len(m.sessions))
	for _, session := range m.sessions {
		sessions = append(sessions, session)
	}
	m.mu.Unlock()
	expired := make([]*httpSession, 0)
	for _, session := range sessions {
		session.mu.Lock()
		lastActivity := session.lastActivity
		closed := session.closed
		session.mu.Unlock()
		if !closed && lastActivity.Before(cutoff) {
			expired = append(expired, session)
		}
	}
	for _, session := range expired {
		session.close()
	}
}

func (m *httpRelayManager) remove(session *httpSession) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if current, exists := m.sessions[session.id]; exists && current == session {
		delete(m.sessions, session.id)
	}
}

func (m *httpRelayManager) closeAll() {
	m.mu.Lock()
	cancel := m.sweepCancel
	m.sweepCancel = nil
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	m.mu.Lock()
	sessions := make([]*httpSession, 0, len(m.sessions))
	for _, session := range m.sessions {
		sessions = append(sessions, session)
	}
	m.mu.Unlock()
	for _, session := range sessions {
		session.close()
	}
}

type httpSession struct {
	mu                   sync.Mutex
	id                   string
	tokenHash            [32]byte
	remoteAddr           net.Addr
	ctx                  context.Context
	cancel               context.CancelFunc
	inbound              chan []byte
	outbound             []sequencedMessage
	nextInboundSequence  uint64
	nextOutboundSequence uint64
	acknowledgedOutbound uint64
	pendingOutboundBytes int
	pollActive           bool
	lastActivity         time.Time
	writeDeadline        time.Time
	notify               chan struct{}
	closed               bool
	closeOnce            sync.Once
	manager              *httpRelayManager
}

func newHTTPSession(id string, tokenHash [32]byte, remoteAddr net.Addr, manager *httpRelayManager) *httpSession {
	config := defaultHTTPRelayConfig()
	if manager != nil {
		config = manager.config
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &httpSession{
		id:           id,
		tokenHash:    tokenHash,
		remoteAddr:   remoteAddr,
		ctx:          ctx,
		cancel:       cancel,
		inbound:      make(chan []byte, config.maxBatchMessages),
		lastActivity: time.Now(),
		notify:       make(chan struct{}, 1),
		manager:      manager,
	}
}

func (s *httpSession) config() httpRelayConfig {
	if s.manager != nil {
		return s.manager.config
	}
	return defaultHTTPRelayConfig()
}

func (s *httpSession) touch() {
	s.mu.Lock()
	s.lastActivity = time.Now()
	s.mu.Unlock()
}

func (s *httpSession) acceptInbound(sequence uint64, payload []byte) error {
	if sequence == 0 {
		return &httpSequenceError{expected: 1, received: sequence}
	}
	config := s.config()
	if len(payload) > config.maxMessageBytes {
		return errHTTPMessageTooLarge
	}
	payloadCopy := append([]byte(nil), payload...)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errHTTPConnClosed
	}
	if sequence <= s.nextInboundSequence {
		return &httpSequenceError{expected: s.nextInboundSequence + 1, received: sequence, duplicate: true}
	}
	expected := s.nextInboundSequence + 1
	if sequence != expected {
		return &httpSequenceError{expected: expected, received: sequence}
	}
	if len(s.inbound) >= cap(s.inbound) {
		return errHTTPPendingLimit
	}
	s.nextInboundSequence = sequence
	s.lastActivity = time.Now()
	s.inbound <- payloadCopy
	return nil
}

func (s *httpSession) enqueueOutbound(payload []byte) error {
	config := s.config()
	payloadCopy := append([]byte(nil), payload...)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errHTTPConnClosed
	}
	if !s.writeDeadline.IsZero() && time.Now().After(s.writeDeadline) {
		return errHTTPWriteDeadline
	}
	if len(s.outbound) >= config.maxBatchMessages || s.pendingOutboundBytes+len(payloadCopy) > config.maxPendingBytes {
		return errHTTPPendingLimit
	}
	s.nextOutboundSequence++
	s.outbound = append(s.outbound, sequencedMessage{
		sequence: s.nextOutboundSequence,
		payload:  payloadCopy,
	})
	s.pendingOutboundBytes += len(payloadCopy)
	s.lastActivity = time.Now()
	select {
	case s.notify <- struct{}{}:
	default:
	}
	return nil
}

func (s *httpSession) acknowledgeOutbound(sequence uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sequence > s.nextOutboundSequence {
		return &httpSequenceError{expected: s.nextOutboundSequence, received: sequence}
	}
	if sequence <= s.acknowledgedOutbound {
		return nil
	}
	remove := 0
	for remove < len(s.outbound) && s.outbound[remove].sequence <= sequence {
		s.pendingOutboundBytes -= len(s.outbound[remove].payload)
		remove++
	}
	if remove > 0 {
		s.outbound = append([]sequencedMessage(nil), s.outbound[remove:]...)
	}
	s.acknowledgedOutbound = sequence
	s.lastActivity = time.Now()
	return nil
}

func (s *httpSession) messagesAfter(after uint64, limit int) ([]sequencedMessage, uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if after > s.nextOutboundSequence {
		return nil, s.nextOutboundSequence, &httpSequenceError{expected: s.nextOutboundSequence, received: after}
	}
	if limit <= 0 {
		limit = len(s.outbound)
	}
	messages := make([]sequencedMessage, 0, limit)
	for _, message := range s.outbound {
		if message.sequence <= after {
			continue
		}
		if len(messages) == limit {
			break
		}
		messages = append(messages, sequencedMessage{
			sequence: message.sequence,
			payload:  append([]byte(nil), message.payload...),
		})
	}
	return messages, s.nextOutboundSequence, nil
}

func (s *httpSession) beginPoll() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errHTTPConnClosed
	}
	if s.pollActive {
		return errHTTPPollActive
	}
	s.pollActive = true
	s.lastActivity = time.Now()
	return nil
}

func (s *httpSession) endPoll() {
	s.mu.Lock()
	s.pollActive = false
	s.mu.Unlock()
}

func (s *httpSession) close() {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		s.cancel()
		if s.manager != nil {
			s.manager.remove(s)
		}
	})
}

type httpConn struct {
	session *httpSession
}

func newHTTPConn(session *httpSession) *httpConn {
	return &httpConn{session: session}
}

func (c *httpConn) ReadMessage() ([]byte, error) {
	select {
	case <-c.session.ctx.Done():
		return nil, io.EOF
	default:
	}
	select {
	case <-c.session.ctx.Done():
		return nil, io.EOF
	case payload := <-c.session.inbound:
		select {
		case <-c.session.ctx.Done():
			return nil, io.EOF
		default:
		}
		return payload, nil
	}
}

func (c *httpConn) WriteMessage(payload []byte) error {
	return c.session.enqueueOutbound(payload)
}

func (c *httpConn) SetWriteDeadline(deadline time.Time) error {
	c.session.mu.Lock()
	defer c.session.mu.Unlock()
	if c.session.closed {
		return errHTTPConnClosed
	}
	c.session.writeDeadline = deadline
	return nil
}

func (c *httpConn) Close() error {
	c.session.close()
	return nil
}

func (c *httpConn) RemoteAddr() net.Addr {
	return c.session.remoteAddr
}

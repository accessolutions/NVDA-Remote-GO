package server

import (
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

type httpTestAddr string

func (a httpTestAddr) Network() string { return "tcp" }
func (a httpTestAddr) String() string  { return string(a) }

func newTestHTTPSession() (*httpSession, *httpRelayManager) {
	config := defaultHTTPRelayConfig()
	config.maxBatchMessages = 3
	config.maxPendingBytes = 32
	manager := newHTTPRelayManager(config)
	session := newHTTPSession("test-session", [32]byte{}, httpTestAddr("127.0.0.1:1234"), manager)
	return session, manager
}

func TestHTTPConnReadMessagePreservesOrder(t *testing.T) {
	session, _ := newTestHTTPSession()
	conn := newHTTPConn(session)
	if err := session.acceptInbound(1, []byte("one")); err != nil {
		t.Fatalf("accepting first message failed: %v", err)
	}
	if err := session.acceptInbound(2, []byte("two")); err != nil {
		t.Fatalf("accepting second message failed: %v", err)
	}
	for _, expected := range []string{"one", "two"} {
		payload, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("reading %q failed: %v", expected, err)
		}
		if string(payload) != expected {
			t.Fatalf("expected %q, got %q", expected, payload)
		}
	}
}

func TestHTTPConnReadMessageUnblocksOnClose(t *testing.T) {
	session, _ := newTestHTTPSession()
	conn := newHTTPConn(session)
	result := make(chan error, 1)
	go func() {
		_, err := conn.ReadMessage()
		result <- err
	}()
	conn.Close()
	select {
	case err := <-result:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("expected EOF after close, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ReadMessage remained blocked after close")
	}
}

func TestHTTPConnWriteMessageAssignsSequences(t *testing.T) {
	session, _ := newTestHTTPSession()
	conn := newHTTPConn(session)
	if err := conn.WriteMessage([]byte("one")); err != nil {
		t.Fatalf("writing first message failed: %v", err)
	}
	if err := conn.WriteMessage([]byte("two")); err != nil {
		t.Fatalf("writing second message failed: %v", err)
	}
	messages, last, err := session.messagesAfter(0, 10)
	if err != nil {
		t.Fatalf("reading outbound messages failed: %v", err)
	}
	if last != 2 || len(messages) != 2 || messages[0].sequence != 1 || messages[1].sequence != 2 {
		t.Fatalf("unexpected outbound sequence: last=%d messages=%+v", last, messages)
	}
}

func TestHTTPSessionAcknowledgementRemovesOnlyConfirmedMessages(t *testing.T) {
	session, _ := newTestHTTPSession()
	conn := newHTTPConn(session)
	for _, payload := range []string{"one", "two", "three"} {
		if err := conn.WriteMessage([]byte(payload)); err != nil {
			t.Fatalf("writing %q failed: %v", payload, err)
		}
	}
	if err := session.acknowledgeOutbound(2); err != nil {
		t.Fatalf("acknowledging messages failed: %v", err)
	}
	messages, _, err := session.messagesAfter(0, 10)
	if err != nil {
		t.Fatalf("reading unacknowledged messages failed: %v", err)
	}
	if len(messages) != 1 || messages[0].sequence != 3 || string(messages[0].payload) != "three" {
		t.Fatalf("unexpected unacknowledged messages: %+v", messages)
	}
}

func TestHTTPSessionPollCanReplayUnacknowledgedMessages(t *testing.T) {
	session, _ := newTestHTTPSession()
	conn := newHTTPConn(session)
	if err := conn.WriteMessage([]byte("one")); err != nil {
		t.Fatalf("writing message failed: %v", err)
	}
	first, _, err := session.messagesAfter(0, 10)
	if err != nil {
		t.Fatalf("first poll failed: %v", err)
	}
	second, _, err := session.messagesAfter(0, 10)
	if err != nil {
		t.Fatalf("repeated poll failed: %v", err)
	}
	if len(first) != 1 || len(second) != 1 || first[0].sequence != second[0].sequence {
		t.Fatalf("unacknowledged message was not replayable: first=%+v second=%+v", first, second)
	}
}

func TestHTTPSessionRejectsSequenceGapsAndIgnoresDuplicates(t *testing.T) {
	session, _ := newTestHTTPSession()
	if err := session.acceptInbound(2, []byte("gap")); err == nil {
		t.Fatal("expected a sequence gap to be rejected")
	} else {
		var sequenceErr *httpSequenceError
		if !errors.As(err, &sequenceErr) || sequenceErr.expected != 1 {
			t.Fatalf("unexpected sequence gap error: %v", err)
		}
	}
	if err := session.acceptInbound(1, []byte("one")); err != nil {
		t.Fatalf("accepting first sequence failed: %v", err)
	}
	if err := session.acceptInbound(1, []byte("duplicate")); err == nil {
		t.Fatal("expected duplicate sequence to be reported")
	} else {
		var sequenceErr *httpSequenceError
		if !errors.As(err, &sequenceErr) || !sequenceErr.duplicate {
			t.Fatalf("unexpected duplicate error: %v", err)
		}
	}
}

func TestHTTPConnRejectsPendingLimitAndHonorsWriteDeadline(t *testing.T) {
	config := defaultHTTPRelayConfig()
	config.maxBatchMessages = 3
	config.maxPendingBytes = 8
	manager := newHTTPRelayManager(config)
	session := newHTTPSession("limited-session", [32]byte{}, httpTestAddr("127.0.0.1:1234"), manager)
	conn := newHTTPConn(session)
	if err := conn.WriteMessage([]byte("12345678")); err != nil {
		t.Fatalf("writing the message at the limit failed: %v", err)
	}
	if err := conn.WriteMessage([]byte("extra")); !errors.Is(err, errHTTPPendingLimit) {
		t.Fatalf("expected pending limit error, got %v", err)
	}
	otherSession, _ := newTestHTTPSession()
	otherConn := newHTTPConn(otherSession)
	if err := otherConn.SetWriteDeadline(time.Now().Add(-time.Second)); err != nil {
		t.Fatalf("setting write deadline failed: %v", err)
	}
	if err := otherConn.WriteMessage([]byte("late")); !errors.Is(err, errHTTPWriteDeadline) {
		t.Fatalf("expected write deadline error, got %v", err)
	}
}

func newLimitedBacklogSession(t *testing.T, backlog int) *httpSession {
	t.Helper()
	config := defaultHTTPRelayConfig()
	config.maxOutboundBacklog = backlog
	manager := newHTTPRelayManager(config)
	return newHTTPSession("backlog-session", [32]byte{}, httpTestAddr("127.0.0.1:1234"), manager)
}

func TestHTTPConnWriteMessageWaitsForBacklogRoom(t *testing.T) {
	session := newLimitedBacklogSession(t, 2)
	conn := newHTTPConn(session)
	if err := conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("setting write deadline failed: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := conn.WriteMessage([]byte("queued")); err != nil {
			t.Fatalf("filling the backlog failed: %v", err)
		}
	}
	result := make(chan error, 1)
	go func() { result <- conn.WriteMessage([]byte("waiting")) }()
	select {
	case err := <-result:
		t.Fatalf("the write returned %v instead of waiting for room", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := session.acknowledgeOutbound(1); err != nil {
		t.Fatalf("acknowledging the first message failed: %v", err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("the write failed once room was released: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the write stayed blocked after room was released")
	}
}

func TestHTTPConnWriteMessageGivesUpAtTheWriteDeadline(t *testing.T) {
	session := newLimitedBacklogSession(t, 1)
	conn := newHTTPConn(session)
	if err := conn.SetWriteDeadline(time.Now().Add(150 * time.Millisecond)); err != nil {
		t.Fatalf("setting write deadline failed: %v", err)
	}
	if err := conn.WriteMessage([]byte("queued")); err != nil {
		t.Fatalf("filling the backlog failed: %v", err)
	}
	if err := conn.WriteMessage([]byte("late")); !errors.Is(err, errHTTPWriteDeadline) {
		t.Fatalf("expected the write deadline to bound the wait, got %v", err)
	}
}

func TestHTTPConnCloseIsIdempotentAndKeepsRemoteAddress(t *testing.T) {
	session, manager := newTestHTTPSession()
	manager.mu.Lock()
	manager.sessions[session.id] = session
	manager.mu.Unlock()
	conn := newHTTPConn(session)
	if got := conn.RemoteAddr().String(); got != "127.0.0.1:1234" {
		t.Fatalf("unexpected remote address %q", got)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("first close failed: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("second close failed: %v", err)
	}
	if manager.get(session.id) != nil {
		t.Fatal("closed session remained in manager")
	}
}

func TestHTTPRelayManagerCreatesRandomSessionCredentials(t *testing.T) {
	manager := newHTTPRelayManager(defaultHTTPRelayConfig())
	first, firstToken, err := manager.create(httpTestAddr("127.0.0.1:1"))
	if err != nil {
		t.Fatalf("creating first session failed: %v", err)
	}
	second, secondToken, err := manager.create(httpTestAddr("127.0.0.1:2"))
	if err != nil {
		t.Fatalf("creating second session failed: %v", err)
	}
	if first.id == second.id || firstToken == secondToken || firstToken == "" || secondToken == "" {
		t.Fatal("session identifiers and tokens must be non-empty and random")
	}
}

func TestHTTPRelayManagerEnforcesPerIPLimit(t *testing.T) {
	config := defaultHTTPRelayConfig()
	config.maxSessionsPerIP = 1
	manager := newHTTPRelayManager(config)
	if _, _, err := manager.create(httpTestAddr("127.0.0.1:1")); err != nil {
		t.Fatalf("creating first session failed: %v", err)
	}
	if _, _, err := manager.create(httpTestAddr("127.0.0.1:2")); err == nil {
		t.Fatal("expected the per-IP session limit to reject the second session")
	}
}

func TestHTTPRelayManagerExpiresInactiveSessions(t *testing.T) {
	config := defaultHTTPRelayConfig()
	config.sessionTTL = time.Second
	manager := newHTTPRelayManager(config)
	session, _, err := manager.create(httpTestAddr("127.0.0.1:1"))
	if err != nil {
		t.Fatalf("creating session failed: %v", err)
	}
	session.mu.Lock()
	session.lastActivity = time.Now().Add(-2 * time.Second)
	session.mu.Unlock()
	manager.expireInactive()
	if manager.get(session.id) != nil {
		t.Fatal("inactive session was not expired")
	}
}

// A session whose owner never talks to the relay (no join, no poll, no
// acknowledgement) must expire even though the relay keeps writing pings to
// it: server-side writes are not a sign of life.
func TestHTTPRelayManagerExpiresSessionKeptBusyOnlyByServerWrites(t *testing.T) {
	config := defaultHTTPRelayConfig()
	config.sessionTTL = time.Second
	manager := newHTTPRelayManager(config)
	session, _, err := manager.create(httpTestAddr("127.0.0.1:1"))
	if err != nil {
		t.Fatalf("creating session failed: %v", err)
	}
	session.mu.Lock()
	session.lastActivity = time.Now().Add(-2 * time.Second)
	session.mu.Unlock()
	conn := newHTTPConn(session)
	if err := conn.WriteMessage(ping_msg); err != nil {
		t.Fatalf("writing the ping failed: %v", err)
	}
	manager.expireInactive()
	if manager.get(session.id) != nil {
		t.Fatal("a session fed only by server pings was not expired")
	}
}

var _ MessageConn = (*httpConn)(nil)
var _ net.Addr = httpTestAddr("")

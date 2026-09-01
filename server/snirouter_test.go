package server

import (
	"crypto/tls"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// startRoutedServer starts a WebSocket server on an ephemeral local port with
// the given TLS routing rules, and returns its address.
func startRoutedServer(t *testing.T, routes SNIRouteList) string {
	t.Helper()

	config, err := gen_cert()
	if err != nil {
		t.Fatalf("unable to generate a test certificate: %v", err)
	}
	config.MinVersion = tls.VersionTLS12

	previousRoutes := sniRoutes
	previousDetect := sniDetectTimeout
	sniRoutes = routes
	sniDetectTimeout = 500 * time.Millisecond

	s := NewWithTLSConfig("127.0.0.1:0", config)
	s.websocket = true
	s.wsPath = "/"
	s.rawFallback = true
	if err := s.Listen(); err != nil {
		sniRoutes = previousRoutes
		sniDetectTimeout = previousDetect
		t.Fatalf("unable to start the test server: %v", err)
	}

	t.Cleanup(func() {
		s.Stop()
		s.Wait()
		sniRoutes = previousRoutes
		sniDetectTimeout = previousDetect
		sl.Lock()
		clients = nil
		channels = nil
		lastID = 0
		sl.Unlock()
	})

	return s.Addr()
}

// startEchoTarget starts a plain TCP listener that echoes everything it
// receives, standing in for the service a routed name is forwarded to.
func startEchoTarget(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("unable to start the target listener: %v", err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	return listener.Addr().String()
}

func TestSNIRouteListSetRejectsInvalidRules(t *testing.T) {
	invalid := []string{
		"turn.example.com",
		"=127.0.0.1:5349",
		"turn.example.com=",
		"turn.example.com=127.0.0.1",
		"turn example.com=127.0.0.1:5349",
	}
	for _, v := range invalid {
		var routes SNIRouteList
		if err := routes.Set(v); err == nil {
			t.Errorf("the rule %q should have been rejected", v)
		}
	}
}

func TestSNIRouteListSetAcceptsValidRules(t *testing.T) {
	var routes SNIRouteList
	if err := routes.Set(" TURN.Example.COM = 127.0.0.1:5349 "); err != nil {
		t.Fatalf("a valid rule was rejected: %v", err)
	}
	if err := routes.Set("other.example.com=[::1]:5349"); err != nil {
		t.Fatalf("a valid rule was rejected: %v", err)
	}
	if err := routes.Set("turn.example.com=127.0.0.1:1234"); err == nil {
		t.Error("a duplicate server name should have been rejected")
	}
	if got := routes.target("Turn.Example.Com."); got != "127.0.0.1:5349" {
		t.Errorf("the lookup should be case and trailing dot insensitive, got %q", got)
	}
	if got := routes.target("unknown.example.com"); got != "" {
		t.Errorf("an unknown name should not be routed, got %q", got)
	}
	if got := routes.target(""); got != "" {
		t.Errorf("an empty name should not be routed, got %q", got)
	}
}

// A connection asking for a routed name must reach the target untouched, so
// that the target can perform its own TLS handshake.
func TestSNIRouteForwardsMatchingName(t *testing.T) {
	target := startEchoTarget(t)
	addr := startRoutedServer(t, SNIRouteList{{Name: "turn.example.com", Target: target}})

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("unable to dial %s: %v", addr, err)
	}
	defer conn.Close()

	// The handshake is expected to fail, since the target echoes the
	// ClientHello back instead of answering it. What matters is that the bytes
	// reached the target at all, which only the echo can explain.
	client := tls.Client(conn, &tls.Config{ServerName: "turn.example.com", InsecureSkipVerify: true})
	_ = client.SetDeadline(time.Now().Add(5 * time.Second))
	err = client.Handshake()
	if err == nil {
		t.Fatal("the handshake should not have succeeded against an echo target")
	}
	if strings.Contains(err.Error(), "certificate") {
		t.Fatalf("the connection reached the server instead of the target: %v", err)
	}
}

// A connection asking for an unrouted name, or for no name at all, must be
// served by the server itself exactly as it was before any routing existed.
func TestSNIRouteServesUnmatchedNames(t *testing.T) {
	target := startEchoTarget(t)
	addr := startRoutedServer(t, SNIRouteList{{Name: "turn.example.com", Target: target}})

	for _, name := range []string{"other.example.com", ""} {
		conn, err := tls.Dial("tcp", addr, &tls.Config{ServerName: name, InsecureSkipVerify: true})
		if err != nil {
			t.Fatalf("the handshake failed for the server name %q: %v", name, err)
		}
		conn.Close()
	}
}

// A legacy client that sends no server name must still be able to speak the
// historic protocol on a routed listener.
func TestSNIRouteKeepsRawProtocol(t *testing.T) {
	target := startEchoTarget(t)
	addr := startRoutedServer(t, SNIRouteList{{Name: "turn.example.com", Target: target}})

	conn, reader := dialRaw(t, addr)
	if _, err := conn.Write([]byte(`{"type":"protocol_version","version":2}` + "\n")); err != nil {
		t.Fatalf("unable to send the protocol version: %v", err)
	}
	if _, err := conn.Write([]byte(`{"type":"join","channel":"snitest","connection_type":"master"}` + "\n")); err != nil {
		t.Fatalf("unable to join a channel: %v", err)
	}
	if got := readTypedMessage(t, reader); got != "channel_joined" {
		t.Fatalf("expected channel_joined, got %q", got)
	}
}

// A client that connects without sending anything must be closed rather than
// held open forever.
func TestSNIRouteClosesSilentClients(t *testing.T) {
	target := startEchoTarget(t)
	addr := startRoutedServer(t, SNIRouteList{{Name: "turn.example.com", Target: target}})

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("unable to dial %s: %v", addr, err)
	}
	defer conn.Close()

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 1)
	if _, err := conn.Read(buf); err == nil {
		t.Fatal("a silent connection should have been closed")
	}
}

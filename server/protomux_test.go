package server

import (
	"bufio"
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// startMuxServer starts a WebSocket server on an ephemeral local port and
// returns its address. The protocol detection timeout is shortened so that the
// tests covering silent clients stay fast.
func startMuxServer(t *testing.T, rawFallback bool) string {
	t.Helper()

	config, err := gen_cert()
	if err != nil {
		t.Fatalf("unable to generate a test certificate: %v", err)
	}
	config.MinVersion = tls.VersionTLS12

	previous := protocolDetectTimeout
	protocolDetectTimeout = 500 * time.Millisecond

	s := NewWithTLSConfig("127.0.0.1:0", config)
	s.websocket = true
	s.wsPath = "/"
	s.rawFallback = rawFallback
	if err := s.Listen(); err != nil {
		protocolDetectTimeout = previous
		t.Fatalf("unable to start the test server: %v", err)
	}

	t.Cleanup(func() {
		s.Stop()
		s.Wait()
		protocolDetectTimeout = previous
		sl.Lock()
		clients = nil
		channels = nil
		lastID = 0
		sl.Unlock()
	})

	return s.Addr()
}

// dialRaw opens a TLS connection and returns it along with a buffered reader,
// mimicking a legacy NVDA Remote client.
func dialRaw(t *testing.T, addr string) (*tls.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatalf("unable to dial %s: %v", addr, err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn, bufio.NewReader(conn)
}

// readTypedMessage reads a newline delimited JSON message and returns its type.
func readTypedMessage(t *testing.T, reader *bufio.Reader) string {
	t.Helper()
	line, err := reader.ReadBytes('\n')
	if err != nil {
		t.Fatalf("unable to read a message: %v", err)
	}
	var data Data
	if err := json.Unmarshal(line, &data); err != nil {
		t.Fatalf("unable to decode %q: %v", line, err)
	}
	return data.Type
}

// waitForClients waits until the expected number of clients is registered.
func waitForClients(t *testing.T, expected int) []*Client {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		list, _ := snapshotClients()
		if len(list) == expected {
			return list
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected %d clients, got %d", expected, len(list))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestMuxRawClient(t *testing.T) {
	addr := startMuxServer(t, true)

	conn, reader := dialRaw(t, addr)
	if _, err := conn.Write([]byte(`{"type":"generate_key"}` + "\n")); err != nil {
		t.Fatalf("unable to send the request: %v", err)
	}
	if got := readTypedMessage(t, reader); got != "generate_key" {
		t.Fatalf("expected a generate_key reply, got %q", got)
	}

	list := waitForClients(t, 1)
	if protocol := list[0].GetProtocol(); protocol != "tcp" {
		t.Fatalf("expected the client to be recorded as tcp, got %q", protocol)
	}
}

func TestMuxWebSocketClient(t *testing.T) {
	addr := startMuxServer(t, true)

	dialer := websocket.Dialer{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		Subprotocols:    []string{"nvdaremote/2.0"},
	}
	conn, _, err := dialer.Dial("wss://"+addr+"/", nil)
	if err != nil {
		t.Fatalf("unable to open the WebSocket connection: %v", err)
	}
	defer conn.Close()

	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"generate_key"}`)); err != nil {
		t.Fatalf("unable to send the request: %v", err)
	}
	_, payload, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("unable to read the reply: %v", err)
	}
	var data Data
	if err := json.Unmarshal(payload, &data); err != nil {
		t.Fatalf("unable to decode %q: %v", payload, err)
	}
	if data.Type != "generate_key" {
		t.Fatalf("expected a generate_key reply, got %q", data.Type)
	}

	list := waitForClients(t, 1)
	if protocol := list[0].GetProtocol(); protocol != "websocket" {
		t.Fatalf("expected the client to be recorded as websocket, got %q", protocol)
	}
}

func TestMuxPlainHTTPRequestIsRejected(t *testing.T) {
	addr := startMuxServer(t, true)

	client := &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
		Timeout:   5 * time.Second,
	}
	resp, err := client.Get("https://" + addr + "/")
	if err != nil {
		t.Fatalf("unable to issue the request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected a 400 status for a non WebSocket request, got %d", resp.StatusCode)
	}
	if list, _ := snapshotClients(); len(list) != 0 {
		t.Fatalf("expected no client to be registered, got %d", len(list))
	}
}

func TestMuxUnknownProtocolIsRejected(t *testing.T) {
	addr := startMuxServer(t, true)

	conn, reader := dialRaw(t, addr)
	if _, err := conn.Write([]byte("BONJOUR\r\n\r\n")); err != nil {
		t.Fatalf("unable to send the request: %v", err)
	}
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("expected an HTTP reply, got %v", err)
	}
	if len(line) < 12 || line[:12] != "HTTP/1.1 400" {
		t.Fatalf("expected a 400 status line, got %q", line)
	}
	if list, _ := snapshotClients(); len(list) != 0 {
		t.Fatalf("expected no client to be registered, got %d", len(list))
	}
}

func TestMuxSilentConnectionIsClosed(t *testing.T) {
	addr := startMuxServer(t, true)

	conn, _ := dialRaw(t, addr)
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("unable to set the read deadline: %v", err)
	}
	buf := make([]byte, 1)
	if _, err := conn.Read(buf); err == nil {
		t.Fatal("expected the server to close a silent connection")
	}
	if list, _ := snapshotClients(); len(list) != 0 {
		t.Fatalf("expected no client to be registered, got %d", len(list))
	}
}

func TestMuxSlowClientIsClassified(t *testing.T) {
	addr := startMuxServer(t, true)

	conn, reader := dialRaw(t, addr)
	request := []byte(`{"type":"generate_key"}` + "\n")
	for _, b := range request {
		if _, err := conn.Write([]byte{b}); err != nil {
			t.Fatalf("unable to send a byte: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := readTypedMessage(t, reader); got != "generate_key" {
		t.Fatalf("expected a generate_key reply, got %q", got)
	}
}

func TestMuxAbortDuringDetection(t *testing.T) {
	addr := startMuxServer(t, true)

	conn, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatalf("unable to dial %s: %v", addr, err)
	}
	if err := conn.Handshake(); err != nil {
		t.Fatalf("the handshake failed: %v", err)
	}
	conn.Close()

	time.Sleep(200 * time.Millisecond)
	if list, _ := snapshotClients(); len(list) != 0 {
		t.Fatalf("expected no client to be registered, got %d", len(list))
	}
}

func TestMuxConcurrentConnections(t *testing.T) {
	addr := startMuxServer(t, true)

	const pairs = 15
	var wg sync.WaitGroup
	errs := make(chan error, pairs*2)

	for i := 0; i < pairs; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			conn, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true})
			if err != nil {
				errs <- err
				return
			}
			defer conn.Close()
			if _, err := conn.Write([]byte(`{"type":"generate_key"}` + "\n")); err != nil {
				errs <- err
				return
			}
			if _, err := bufio.NewReader(conn).ReadBytes('\n'); err != nil {
				errs <- err
			}
		}()
		go func() {
			defer wg.Done()
			dialer := websocket.Dialer{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
				Subprotocols:    []string{"nvdaremote/2.0"},
			}
			conn, _, err := dialer.Dial("wss://"+addr+"/", nil)
			if err != nil {
				errs <- err
				return
			}
			defer conn.Close()
			if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"generate_key"}`)); err != nil {
				errs <- err
				return
			}
			if _, _, err := conn.ReadMessage(); err != nil {
				errs <- err
			}
		}()
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("a concurrent connection failed: %v", err)
	}
}

func TestMuxRawFallbackDisabled(t *testing.T) {
	addr := startMuxServer(t, false)

	conn, reader := dialRaw(t, addr)
	if _, err := conn.Write([]byte(`{"type":"generate_key"}` + "\n")); err != nil {
		t.Fatalf("unable to send the request: %v", err)
	}
	line, err := reader.ReadString('\n')
	if err != nil && err != io.EOF {
		t.Fatalf("expected an HTTP reply or a closed connection, got %v", err)
	}
	if len(line) >= 12 && line[:12] == "HTTP/1.1 400" {
		return
	}
	if line == "" {
		return
	}
	t.Fatalf("expected the historic protocol to be refused, got %q", line)
}

func TestPeekedConnReplaysBufferedBytes(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()

	go func() {
		_, _ = left.Write([]byte("HELLO WORLD"))
	}()

	reader := bufio.NewReader(right)
	if _, err := reader.Peek(1); err != nil {
		t.Fatalf("unable to peek: %v", err)
	}
	wrapped := &peekedConn{Conn: right, reader: reader}
	buf := make([]byte, 11)
	if _, err := io.ReadFull(wrapped, buf); err != nil {
		t.Fatalf("unable to read through the wrapper: %v", err)
	}
	if string(buf) != "HELLO WORLD" {
		t.Fatalf("expected the peeked bytes to be replayed, got %q", buf)
	}
	if wrapped.RemoteAddr() == nil {
		t.Fatal("expected the remote address to be forwarded")
	}
}

package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func startHTTPRelayV2TestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	s := NewWebSocketServer("127.0.0.1:0", nil, "/")
	s.httpRelayEnabled = true
	s.httpRelayV2Enabled = true
	s.httpRelayPath = "/remote/http/v1"
	s.httpRelayV2Path = "/remote/http/v2"
	config := defaultHTTPRelayConfig()
	config.streamKeepalive = time.Second
	s.httpRelayManager = newHTTPRelayManager(config)
	s.ctx, s.Stop = context.WithCancel(context.Background())
	mux := http.NewServeMux()
	registerHTTPRelayRoutes(mux, s, s.httpRelayPath)
	registerHTTPRelayV2Routes(mux, s, s.httpRelayV2Path)
	testServer := httptest.NewServer(mux)
	t.Cleanup(func() {
		testServer.Close()
		s.httpRelayManager.closeAll()
		s.Stop()
		s.Wait()
		sl.Lock()
		clients = nil
		channels = nil
		lastID = 0
		sl.Unlock()
	})
	return s, testServer
}

func createHTTPRelayV2Session(t *testing.T, testServer *httptest.Server) (string, string, httpCreateSessionResponseV2) {
	t.Helper()
	request, err := http.NewRequest(
		http.MethodPost,
		testServer.URL+"/remote/http/v2/sessions",
		strings.NewReader(`{"protocol":"nvdaremote-http/2"}`),
	)
	if err != nil {
		t.Fatalf("creating v2 session request failed: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := testServer.Client().Do(request)
	if err != nil {
		t.Fatalf("creating v2 session failed: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("expected v2 session status 201, got %d: %s", response.StatusCode, body)
	}
	var payload httpCreateSessionResponseV2
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatalf("decoding v2 session response failed: %v", err)
	}
	if payload.SessionID == "" || payload.Token == "" {
		t.Fatal("v2 session response did not contain credentials")
	}
	return payload.SessionID, payload.Token, payload
}

func openHTTPRelayV2Stream(t *testing.T, testServer *httptest.Server, sessionID, token string, after uint64) (*http.Response, *bufio.Reader) {
	t.Helper()
	url := testServer.URL + "/remote/http/v2/sessions/" + sessionID + "/stream?after=" + strconv.FormatUint(after, 10)
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("creating v2 stream request failed: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "text/event-stream")
	response, err := testServer.Client().Do(request)
	if err != nil {
		t.Fatalf("opening v2 stream failed: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("expected v2 stream status 200, got %d: %s", response.StatusCode, body)
	}
	return response, bufio.NewReader(response.Body)
}

func readHTTPRelayV2Block(reader *bufio.Reader) (string, error) {
	var block strings.Builder
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return "", err
		}
		if line == "\n" || line == "\r\n" {
			return block.String(), nil
		}
		block.WriteString(line)
	}
}

func readHTTPRelayV2BlockWithTimeout(reader *bufio.Reader, timeout time.Duration) (string, error) {
	type result struct {
		block string
		err   error
	}
	results := make(chan result, 1)
	go func() {
		block, err := readHTTPRelayV2Block(reader)
		results <- result{block: block, err: err}
	}()
	select {
	case result := <-results:
		return result.block, result.err
	case <-time.After(timeout):
		return "", fmt.Errorf("timed out after %s waiting for an SSE block", timeout)
	}
}

func readHTTPRelayV2Ready(t *testing.T, reader *bufio.Reader) {
	t.Helper()
	block, err := readHTTPRelayV2BlockWithTimeout(reader, time.Second)
	if err != nil {
		t.Fatalf("reading ready event failed: %v", err)
	}
	if block != "event: ready\ndata: {}\n" {
		t.Fatalf("unexpected ready event: %q", block)
	}
}

func postHTTPRelayV2Acknowledgement(t *testing.T, testServer *httptest.Server, sessionID, token string, receivedThrough uint64) *http.Response {
	t.Helper()
	body := `{"messages":[],"receivedThrough":` + strconv.FormatUint(receivedThrough, 10) + `}`
	url := testServer.URL + "/remote/http/v2/sessions/" + sessionID + "/messages"
	request, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("creating v2 acknowledgement request failed: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response, err := testServer.Client().Do(request)
	if err != nil {
		t.Fatalf("sending v2 acknowledgement failed: %v", err)
	}
	return response
}

func TestHTTPRelayV2CreateSessionReturnsKeepalive(t *testing.T) {
	_, testServer := startHTTPRelayV2TestServer(t)
	_, _, payload := createHTTPRelayV2Session(t, testServer)
	if payload.Protocol != httpRelayProtocolV2 {
		t.Fatalf("unexpected v2 protocol %q", payload.Protocol)
	}
	if payload.StreamKeepaliveSeconds != 1 {
		t.Fatalf("expected one-second stream keepalive, got %d", payload.StreamKeepaliveSeconds)
	}
}

func TestHTTPRelayV2CreateSessionRejectsV1Protocol(t *testing.T) {
	_, testServer := startHTTPRelayV2TestServer(t)
	request, err := http.NewRequest(
		http.MethodPost,
		testServer.URL+"/remote/http/v2/sessions",
		strings.NewReader(`{"protocol":"nvdaremote-http/1"}`),
	)
	if err != nil {
		t.Fatalf("creating request failed: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := testServer.Client().Do(request)
	if err != nil {
		t.Fatalf("sending request failed: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for the v1 protocol, got %d", response.StatusCode)
	}
}

func TestHTTPRelayV2StreamSendsReadyFirst(t *testing.T) {
	_, testServer := startHTTPRelayV2TestServer(t)
	sessionID, token, _ := createHTTPRelayV2Session(t, testServer)
	response, reader := openHTTPRelayV2Stream(t, testServer, sessionID, token, 0)
	defer response.Body.Close()
	readHTTPRelayV2Ready(t, reader)
	if !strings.Contains(response.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("unexpected stream content type %q", response.Header.Get("Content-Type"))
	}
	if response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("X-Accel-Buffering") != "no" {
		t.Fatal("stream buffering headers are missing")
	}
}

func TestHTTPRelayV2StreamRequiresBearerToken(t *testing.T) {
	_, testServer := startHTTPRelayV2TestServer(t)
	sessionID, _, _ := createHTTPRelayV2Session(t, testServer)
	request, err := http.NewRequest(http.MethodGet, testServer.URL+"/remote/http/v2/sessions/"+sessionID+"/stream", nil)
	if err != nil {
		t.Fatalf("creating stream request failed: %v", err)
	}
	response, err := testServer.Client().Do(request)
	if err != nil {
		t.Fatalf("sending stream request failed: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 without a bearer token, got %d", response.StatusCode)
	}
}

func TestHTTPRelayV2StreamDeliversPushedMessage(t *testing.T) {
	s, testServer := startHTTPRelayV2TestServer(t)
	sessionID, token, _ := createHTTPRelayV2Session(t, testServer)
	response, reader := openHTTPRelayV2Stream(t, testServer, sessionID, token, 0)
	defer response.Body.Close()
	readHTTPRelayV2Ready(t, reader)
	session := s.httpRelay().get(sessionID)
	if session == nil {
		t.Fatal("created v2 session was not found")
	}
	if err := session.enqueueOutbound([]byte(`{"type":"ping"}`)); err != nil {
		t.Fatalf("queueing outbound message failed: %v", err)
	}
	block, err := readHTTPRelayV2BlockWithTimeout(reader, time.Second)
	if err != nil {
		t.Fatalf("reading pushed message failed: %v", err)
	}
	if block != "id: 1\ndata: {\"type\":\"ping\"}\n" {
		t.Fatalf("unexpected pushed message block: %q", block)
	}
}

func TestHTTPRelayV2StreamReplaysAfterSequence(t *testing.T) {
	s, testServer := startHTTPRelayV2TestServer(t)
	sessionID, token, _ := createHTTPRelayV2Session(t, testServer)
	session := s.httpRelay().get(sessionID)
	if session == nil {
		t.Fatal("created v2 session was not found")
	}
	if err := session.enqueueOutbound([]byte(`{"type":"old"}`)); err != nil {
		t.Fatalf("queueing first outbound message failed: %v", err)
	}
	if err := session.enqueueOutbound([]byte(`{"type":"new"}`)); err != nil {
		t.Fatalf("queueing second outbound message failed: %v", err)
	}
	response, reader := openHTTPRelayV2Stream(t, testServer, sessionID, token, 1)
	defer response.Body.Close()
	readHTTPRelayV2Ready(t, reader)
	block, err := readHTTPRelayV2BlockWithTimeout(reader, time.Second)
	if err != nil {
		t.Fatalf("reading replayed message failed: %v", err)
	}
	if block != "id: 2\ndata: {\"type\":\"new\"}\n" {
		t.Fatalf("unexpected replayed message block: %q", block)
	}
}

func TestHTTPRelayV2StreamRejectsSecondReader(t *testing.T) {
	_, testServer := startHTTPRelayV2TestServer(t)
	sessionID, token, _ := createHTTPRelayV2Session(t, testServer)
	firstResponse, firstReader := openHTTPRelayV2Stream(t, testServer, sessionID, token, 0)
	defer firstResponse.Body.Close()
	readHTTPRelayV2Ready(t, firstReader)
	url := testServer.URL + "/remote/http/v2/sessions/" + sessionID + "/stream"
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("creating second stream request failed: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	secondResponse, err := testServer.Client().Do(request)
	if err != nil {
		t.Fatalf("opening second stream failed: %v", err)
	}
	defer secondResponse.Body.Close()
	if secondResponse.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 for a second reader, got %d", secondResponse.StatusCode)
	}
}

func TestHTTPRelayV2StreamSendsKeepalive(t *testing.T) {
	s, testServer := startHTTPRelayV2TestServer(t)
	s.httpRelayManager.config.streamKeepalive = 20 * time.Millisecond
	sessionID, token, _ := createHTTPRelayV2Session(t, testServer)
	response, reader := openHTTPRelayV2Stream(t, testServer, sessionID, token, 0)
	defer response.Body.Close()
	readHTTPRelayV2Ready(t, reader)
	block, err := readHTTPRelayV2BlockWithTimeout(reader, time.Second)
	if err != nil {
		t.Fatalf("reading keepalive failed: %v", err)
	}
	if block != ": keepalive\n" {
		t.Fatalf("unexpected keepalive block: %q", block)
	}
}

func TestHTTPRelayV2StreamCompactsPayload(t *testing.T) {
	s, testServer := startHTTPRelayV2TestServer(t)
	sessionID, token, _ := createHTTPRelayV2Session(t, testServer)
	session := s.httpRelay().get(sessionID)
	if session == nil {
		t.Fatal("created v2 session was not found")
	}
	if err := session.enqueueOutbound([]byte("{\n  \"type\": \"ping\"\n}")); err != nil {
		t.Fatalf("queueing multiline outbound message failed: %v", err)
	}
	response, reader := openHTTPRelayV2Stream(t, testServer, sessionID, token, 0)
	defer response.Body.Close()
	readHTTPRelayV2Ready(t, reader)
	block, err := readHTTPRelayV2BlockWithTimeout(reader, time.Second)
	if err != nil {
		t.Fatalf("reading compacted message failed: %v", err)
	}
	if block != "id: 1\ndata: {\"type\":\"ping\"}\n" {
		t.Fatalf("unexpected compacted message block: %q", block)
	}
}

func TestHTTPRelayV2StreamKeepsSessionAlive(t *testing.T) {
	s, testServer := startHTTPRelayV2TestServer(t)
	s.httpRelayManager.config.sessionTTL = 60 * time.Millisecond
	s.httpRelayManager.config.streamKeepalive = 15 * time.Millisecond
	s.httpRelayManager.start()
	sessionID, token, _ := createHTTPRelayV2Session(t, testServer)
	response, reader := openHTTPRelayV2Stream(t, testServer, sessionID, token, 0)
	defer response.Body.Close()
	readHTTPRelayV2Ready(t, reader)
	if block, err := readHTTPRelayV2BlockWithTimeout(reader, time.Second); err != nil || block != ": keepalive\n" {
		t.Fatalf("reading initial keepalive failed: block=%q error=%v", block, err)
	}
	time.Sleep(150 * time.Millisecond)
	if s.httpRelay().get(sessionID) == nil {
		t.Fatal("silent streaming session expired despite keepalives")
	}
}

func TestHTTPRelayV2AcknowledgesReceivedThrough(t *testing.T) {
	s, testServer := startHTTPRelayV2TestServer(t)
	sessionID, token, _ := createHTTPRelayV2Session(t, testServer)
	session := s.httpRelay().get(sessionID)
	if session == nil {
		t.Fatal("created v2 session was not found")
	}
	if err := session.enqueueOutbound([]byte(`{"type":"ping"}`)); err != nil {
		t.Fatalf("queueing outbound message failed: %v", err)
	}
	response := postHTTPRelayV2Acknowledgement(t, testServer, sessionID, token, 1)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("expected acknowledgement status 200, got %d: %s", response.StatusCode, body)
	}
	session.mu.Lock()
	pendingBytes := session.pendingOutboundBytes
	acknowledged := session.acknowledgedOutbound
	session.mu.Unlock()
	if pendingBytes != 0 || acknowledged != 1 {
		t.Fatalf("acknowledgement did not release outbound data: pending=%d acknowledged=%d", pendingBytes, acknowledged)
	}
}

func TestHTTPRelayV2AcceptsAcknowledgementOnlyBatch(t *testing.T) {
	_, testServer := startHTTPRelayV2TestServer(t)
	sessionID, token, _ := createHTTPRelayV2Session(t, testServer)
	response := postHTTPRelayV2Acknowledgement(t, testServer, sessionID, token, 0)
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for an acknowledgement without a sequence, got %d", response.StatusCode)
	}
	response = postHTTPRelayV2Acknowledgement(t, testServer, sessionID, token, 1)
	defer response.Body.Close()
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 for an acknowledgement ahead of the server, got %d", response.StatusCode)
	}
}

func TestHTTPRelayV1RoutesAreUnchanged(t *testing.T) {
	_, testServer := startHTTPRelayV2TestServer(t)
	sessionID, token := createHTTPRelaySession(t, testServer)
	accepted := postHTTPRelayMessages(t, testServer, sessionID, token, []httpInboundMessage{
		{Sequence: 1, Payload: json.RawMessage(`{"type":"protocol_version","version":2}`)},
	})
	if accepted != 1 {
		t.Fatalf("v1 route accepted sequence %d, want 1", accepted)
	}
}

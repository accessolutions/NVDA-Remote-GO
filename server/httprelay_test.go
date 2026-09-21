package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func startHTTPRelayTestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	s := NewWebSocketServer("127.0.0.1:0", nil, "/")
	s.httpRelayPath = "/remote/http/v1"
	s.httpRelayManager = newHTTPRelayManager(defaultHTTPRelayConfig())
	s.ctx, s.Stop = context.WithCancel(context.Background())
	mux := http.NewServeMux()
	registerHTTPRelayRoutes(mux, s, s.httpRelayPath)
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

func createHTTPRelaySession(t *testing.T, testServer *httptest.Server) (string, string) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, testServer.URL+"/remote/http/v1/sessions", strings.NewReader(`{"protocol":"nvdaremote-http/1"}`))
	if err != nil {
		t.Fatalf("creating session request failed: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := testServer.Client().Do(request)
	if err != nil {
		t.Fatalf("creating session failed: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("expected session creation status 201, got %d: %s", response.StatusCode, body)
	}
	var payload httpCreateSessionResponse
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatalf("decoding session response failed: %v", err)
	}
	if payload.SessionID == "" || payload.Token == "" {
		t.Fatal("session response did not contain credentials")
	}
	return payload.SessionID, payload.Token
}

func TestHTTPRelayCreateSessionReturnsCredentials(t *testing.T) {
	_, testServer := startHTTPRelayTestServer(t)
	request, err := http.NewRequest(http.MethodPost, testServer.URL+"/remote/http/v1/sessions", strings.NewReader(`{"protocol":"nvdaremote-http/1"}`))
	if err != nil {
		t.Fatalf("creating request failed: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := testServer.Client().Do(request)
	if err != nil {
		t.Fatalf("sending request failed: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", response.StatusCode)
	}
	if response.Header.Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatalf("unexpected content type %q", response.Header.Get("Content-Type"))
	}
	if response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("required response security headers are missing")
	}
	if response.Header.Get("Upgrade") != "" {
		t.Fatal("HTTP relay response must not contain an Upgrade header")
	}
	var payload httpCreateSessionResponse
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatalf("decoding response failed: %v", err)
	}
	if payload.SessionID == "" || payload.Token == "" || payload.Protocol != httpRelayProtocol {
		t.Fatalf("unexpected session response: %+v", payload)
	}
}

func TestHTTPRelayRejectsUpgradeFreeRequestWithWrongContentType(t *testing.T) {
	_, testServer := startHTTPRelayTestServer(t)
	request, err := http.NewRequest(http.MethodPost, testServer.URL+"/remote/http/v1/sessions", strings.NewReader(`{"protocol":"nvdaremote-http/1"}`))
	if err != nil {
		t.Fatalf("creating request failed: %v", err)
	}
	request.Header.Set("Content-Type", "text/plain")
	if request.Header.Get("Upgrade") != "" || request.Header.Get("Connection") != "" {
		t.Fatal("test request unexpectedly contains WebSocket headers")
	}
	response, err := testServer.Client().Do(request)
	if err != nil {
		t.Fatalf("sending request failed: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("expected 415, got %d", response.StatusCode)
	}
}

func TestHTTPRelayRejectsOversizedCreationBody(t *testing.T) {
	_, testServer := startHTTPRelayTestServer(t)
	body := `{"protocol":"nvdaremote-http/1","padding":"` + strings.Repeat("x", 5000) + `"}`
	request, err := http.NewRequest(http.MethodPost, testServer.URL+"/remote/http/v1/sessions", strings.NewReader(body))
	if err != nil {
		t.Fatalf("creating request failed: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := testServer.Client().Do(request)
	if err != nil {
		t.Fatalf("sending request failed: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413 for an oversized JSON body, got %d", response.StatusCode)
	}
}

func TestHTTPRelayRequiresBearerToken(t *testing.T) {
	_, testServer := startHTTPRelayTestServer(t)
	sessionID, _ := createHTTPRelaySession(t, testServer)
	request, err := http.NewRequest(http.MethodGet, testServer.URL+"/remote/http/v1/sessions/"+sessionID+"/messages?wait=1", nil)
	if err != nil {
		t.Fatalf("creating poll request failed: %v", err)
	}
	response, err := testServer.Client().Do(request)
	if err != nil {
		t.Fatalf("sending poll request failed: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 without a bearer token, got %d", response.StatusCode)
	}
}

func TestHTTPRelayCloseIsIdempotent(t *testing.T) {
	_, testServer := startHTTPRelayTestServer(t)
	sessionID, token := createHTTPRelaySession(t, testServer)
	url := testServer.URL + "/remote/http/v1/sessions/" + sessionID
	for attempt := 0; attempt < 2; attempt++ {
		request, err := http.NewRequest(http.MethodDelete, url, nil)
		if err != nil {
			t.Fatalf("creating close request failed: %v", err)
		}
		request.Header.Set("Authorization", "Bearer "+token)
		response, err := testServer.Client().Do(request)
		if err != nil {
			t.Fatalf("sending close request failed: %v", err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusNoContent {
			t.Fatalf("close attempt %d returned %d", attempt+1, response.StatusCode)
		}
	}
}

func TestHTTPRelayRejectsWrongMethod(t *testing.T) {
	_, testServer := startHTTPRelayTestServer(t)
	request, err := http.NewRequest(http.MethodGet, testServer.URL+"/remote/http/v1/sessions", nil)
	if err != nil {
		t.Fatalf("creating request failed: %v", err)
	}
	response, err := testServer.Client().Do(request)
	if err != nil {
		t.Fatalf("sending request failed: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", response.StatusCode)
	}
	if response.Header.Get("Allow") != http.MethodPost {
		t.Fatalf("expected Allow: POST, got %q", response.Header.Get("Allow"))
	}
}

func postHTTPRelayMessages(t *testing.T, testServer *httptest.Server, sessionID, token string, messages []httpInboundMessage) uint64 {
	t.Helper()
	body, err := json.Marshal(httpInboundRequest{Messages: messages})
	if err != nil {
		t.Fatalf("encoding message batch failed: %v", err)
	}
	url := testServer.URL + "/remote/http/v1/sessions/" + sessionID + "/messages"
	request, err := http.NewRequest(http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("creating message request failed: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response, err := testServer.Client().Do(request)
	if err != nil {
		t.Fatalf("sending message batch failed: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("expected message batch status 200, got %d: %s", response.StatusCode, body)
	}
	var result struct {
		AcceptedThrough uint64 `json:"acceptedThrough"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatalf("decoding message response failed: %v", err)
	}
	return result.AcceptedThrough
}

func postHTTPRelayAcknowledgement(t *testing.T, testServer *httptest.Server, sessionID, token string, receivedThrough uint64) *http.Response {
	t.Helper()
	body := `{"messages":[],"receivedThrough":` + strconv.FormatUint(receivedThrough, 10) + `}`
	url := testServer.URL + "/remote/http/v1/sessions/" + sessionID + "/messages"
	request, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("creating acknowledgement request failed: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response, err := testServer.Client().Do(request)
	if err != nil {
		t.Fatalf("sending acknowledgement failed: %v", err)
	}
	return response
}

func TestHTTPRelayAcceptsAcknowledgementOnlyBatch(t *testing.T) {
	s, testServer := startHTTPRelayTestServer(t)
	sessionID, token := createHTTPRelaySession(t, testServer)
	session := s.httpRelay().get(sessionID)
	if session == nil {
		t.Fatal("created session was not found")
	}
	if err := session.enqueueOutbound([]byte(`{"type":"ping"}`)); err != nil {
		t.Fatalf("queueing outbound message failed: %v", err)
	}
	response := postHTTPRelayAcknowledgement(t, testServer, sessionID, token, 1)
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

func TestHTTPRelayRejectsAcknowledgementAheadOfServer(t *testing.T) {
	_, testServer := startHTTPRelayTestServer(t)
	sessionID, token := createHTTPRelaySession(t, testServer)
	response := postHTTPRelayAcknowledgement(t, testServer, sessionID, token, 1)
	defer response.Body.Close()
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("expected acknowledgement status 409, got %d", response.StatusCode)
	}
	var result struct {
		Error    string `json:"error"`
		Expected uint64 `json:"expectedSequence"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatalf("decoding acknowledgement error failed: %v", err)
	}
	if result.Error != "invalid_received_through" || result.Expected != 0 {
		t.Fatalf("unexpected acknowledgement error: %+v", result)
	}
}

func pollHTTPRelayMessages(t *testing.T, testServer *httptest.Server, sessionID, token string, after uint64) httpPollResponse {
	t.Helper()
	url := testServer.URL + "/remote/http/v1/sessions/" + sessionID + "/messages?after=" + strconv.FormatUint(after, 10) + "&wait=1"
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("creating poll request failed: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := testServer.Client().Do(request)
	if err != nil {
		t.Fatalf("sending poll request failed: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("expected poll status 200, got %d: %s", response.StatusCode, body)
	}
	var result httpPollResponse
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatalf("decoding poll response failed: %v", err)
	}
	return result
}

func TestHTTPRelayIntegratesWithTwoClients(t *testing.T) {
	_, testServer := startHTTPRelayTestServer(t)
	firstID, firstToken := createHTTPRelaySession(t, testServer)
	secondID, secondToken := createHTTPRelaySession(t, testServer)

	postHTTPRelayMessages(t, testServer, firstID, firstToken, []httpInboundMessage{
		{Sequence: 1, Payload: json.RawMessage(`{"type":"protocol_version","version":2}`)},
		{Sequence: 2, Payload: json.RawMessage(`{"type":"join","channel":"http-test","connection_type":"slave"}`)},
	})
	postHTTPRelayMessages(t, testServer, secondID, secondToken, []httpInboundMessage{
		{Sequence: 1, Payload: json.RawMessage(`{"type":"protocol_version","version":2}`)},
		{Sequence: 2, Payload: json.RawMessage(`{"type":"join","channel":"http-test","connection_type":"master"}`)},
	})

	firstPoll := pollHTTPRelayMessages(t, testServer, firstID, firstToken, 0)
	secondPoll := pollHTTPRelayMessages(t, testServer, secondID, secondToken, 0)
	if !containsHTTPMessageType(firstPoll.Messages, "client_joined") {
		t.Fatalf("first client did not receive client_joined: %+v", firstPoll.Messages)
	}
	if !containsHTTPMessageType(secondPoll.Messages, "channel_joined") {
		t.Fatalf("second client did not receive channel_joined: %+v", secondPoll.Messages)
	}

	postHTTPRelayMessages(t, testServer, secondID, secondToken, []httpInboundMessage{
		{Sequence: 3, Payload: json.RawMessage(`{"type":"ping"}`)},
	})
	forwarded := pollHTTPRelayMessages(t, testServer, firstID, firstToken, firstPoll.LastSequence)
	if !containsHTTPMessageType(forwarded.Messages, "ping") {
		t.Fatalf("first client did not receive the relayed message: %+v", forwarded.Messages)
	}
}

func TestHTTPRelayRejectsSequenceGap(t *testing.T) {
	_, testServer := startHTTPRelayTestServer(t)
	sessionID, token := createHTTPRelaySession(t, testServer)
	url := testServer.URL + "/remote/http/v1/sessions/" + sessionID + "/messages"
	body := `{"messages":[{"sequence":2,"payload":{"type":"protocol_version","version":2}}]}`
	request, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("creating gap request failed: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response, err := testServer.Client().Do(request)
	if err != nil {
		t.Fatalf("sending gap request failed: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 for a sequence gap, got %d", response.StatusCode)
	}
	var result struct {
		Expected uint64 `json:"expectedSequence"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatalf("decoding sequence error failed: %v", err)
	}
	if result.Expected != 1 {
		t.Fatalf("expected next sequence 1, got %d", result.Expected)
	}
}

func TestHTTPRelayDuplicatePostIsIdempotent(t *testing.T) {
	_, testServer := startHTTPRelayTestServer(t)
	sessionID, token := createHTTPRelaySession(t, testServer)
	message := []httpInboundMessage{{Sequence: 1, Payload: json.RawMessage(`{"type":"protocol_version","version":2}`)}}
	first := postHTTPRelayMessages(t, testServer, sessionID, token, message)
	second := postHTTPRelayMessages(t, testServer, sessionID, token, message)
	if first != 1 || second != 1 {
		t.Fatalf("duplicate post changed accepted sequence: first=%d second=%d", first, second)
	}
}

func TestHTTPRelayRoutesAreRegisteredOnTLSListener(t *testing.T) {
	config, err := gen_cert()
	if err != nil {
		t.Fatalf("generating test certificate failed: %v", err)
	}
	s := NewWebSocketServer("127.0.0.1:0", config, "/")
	s.rawFallback = false
	s.httpRelayEnabled = true
	s.httpRelayPath = "/remote/http/v1"
	s.httpRelayManager = newHTTPRelayManager(defaultHTTPRelayConfig())
	if err := s.Listen(); err != nil {
		t.Fatalf("starting TLS listener failed: %v", err)
	}
	t.Cleanup(func() {
		s.Stop()
		s.Wait()
		sl.Lock()
		clients = nil
		channels = nil
		lastID = 0
		sl.Unlock()
	})
	client := &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}
	request, err := http.NewRequest(http.MethodPost, "https://"+s.Addr()+"/remote/http/v1/sessions", strings.NewReader(`{"protocol":"nvdaremote-http/1"}`))
	if err != nil {
		t.Fatalf("creating TLS request failed: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("sending TLS request failed: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 from the TLS listener, got %d", response.StatusCode)
	}
}

func containsHTTPMessageType(messages []httpOutboundMessage, expected string) bool {
	for _, message := range messages {
		var payload struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(message.Payload, &payload) == nil && payload.Type == expected {
			return true
		}
	}
	return false
}

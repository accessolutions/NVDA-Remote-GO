package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// resetAdmissionState clears the admin password and the block list between tests.
func resetAdmissionState(t *testing.T) {
	t.Helper()
	previousEnabled := adminEnabled
	t.Cleanup(func() {
		adminEnabled = previousEnabled
		adminPasswordMu.Lock()
		adminPasswordHash = nil
		adminPasswordMu.Unlock()
		replaceBlockList(nil)
	})
	adminPasswordMu.Lock()
	adminPasswordHash = nil
	adminPasswordMu.Unlock()
	replaceBlockList(nil)
}

func postHTTPRelaySession(t *testing.T, testServer *httptest.Server) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, testServer.URL+"/remote/http/v1/sessions", strings.NewReader(`{"protocol":"nvdaremote-http/1"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := testServer.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

// joinedClient opens a relay session, joins the given channel and returns the
// server side client once the join has been processed.
func joinedClient(t *testing.T, testServer *httptest.Server, channel string) *Client {
	t.Helper()
	before, _ := snapshotClients()
	known := make(map[*Client]bool, len(before))
	for _, c := range before {
		known[c] = true
	}
	sessionID, token := createHTTPRelaySession(t, testServer)
	postHTTPRelayMessages(t, testServer, sessionID, token, []httpInboundMessage{
		{Sequence: 1, Payload: json.RawMessage(`{"type":"protocol_version","version":2}`)},
		{Sequence: 2, Payload: json.RawMessage(`{"type":"join","channel":"` + channel + `","connection_type":"slave"}`)},
	})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		list, _ := snapshotClients()
		for _, c := range list {
			if !known[c] && c.GetChannel() != nil {
				return c
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the client did not join the channel in time")
	return nil
}

// stillConnected reports whether a client that was told to join is still there
// after the server had time to process the join.
func stillConnected(t *testing.T, testServer *httptest.Server, channel string) bool {
	t.Helper()
	before, _ := snapshotClients()
	known := make(map[*Client]bool, len(before))
	for _, c := range before {
		known[c] = true
	}
	sessionID, token := createHTTPRelaySession(t, testServer)
	postHTTPRelayMessages(t, testServer, sessionID, token, []httpInboundMessage{
		{Sequence: 1, Payload: json.RawMessage(`{"type":"protocol_version","version":2}`)},
		{Sequence: 2, Payload: json.RawMessage(`{"type":"join","channel":"` + channel + `","connection_type":"slave"}`)},
	})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		list, _ := snapshotClients()
		var fresh *Client
		for _, c := range list {
			if !known[c] {
				fresh = c
			}
		}
		if fresh == nil {
			return false
		}
		if fresh.GetChannel() != nil {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the join outcome could not be observed in time")
	return false
}

func waitDisconnected(t *testing.T, c *Client) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for FindClient(c) {
		if time.Now().After(deadline) {
			t.Fatal("the client was not disconnected")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestConnectionRefusalRules(t *testing.T) {
	resetAdmissionState(t)
	adminEnabled = true

	if reason := connectionRefusal("192.0.2.1"); !strings.Contains(reason, "password") {
		t.Fatalf("connections should be refused before the admin password is set, got %q", reason)
	}
	if err := setAdminPassword("correct horse battery"); err != nil {
		t.Fatal(err)
	}
	if reason := connectionRefusal("192.0.2.1"); reason != "" {
		t.Fatalf("connections should be accepted once the password is set, got %q", reason)
	}
	replaceBlockList([]blockedClient{{IP: "192.0.2.1", Channel: "alpha", ClientID: 7, BlockedAt: time.Now()}})
	if reason := connectionRefusal("192.0.2.1"); reason != "" {
		t.Fatalf("a block must not act before the channel is known, got %q", reason)
	}
	if reason := joinRefusal("192.0.2.1", "alpha"); !strings.Contains(reason, "blocked") {
		t.Fatalf("the blocked address must be refused on its channel, got %q", reason)
	}
	if reason := joinRefusal("192.0.2.1", "beta"); reason != "" {
		t.Fatalf("the same address must be accepted on another channel, got %q", reason)
	}
	if reason := joinRefusal("192.0.2.2", "alpha"); reason != "" {
		t.Fatalf("another address must be accepted on the blocked channel, got %q", reason)
	}
	if isBlocked("", "alpha") || isBlocked("192.0.2.1", "") {
		t.Fatal("an empty address or channel must never match the block list")
	}
}

func TestHTTPRelayRefusesClientsUntilPasswordDefined(t *testing.T) {
	resetAdmissionState(t)
	adminEnabled = true
	_, testServer := startHTTPRelayTestServer(t)

	response := postHTTPRelaySession(t, testServer)
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 before the password is set, got %d: %s", response.StatusCode, body)
	}
	if !strings.Contains(string(body), "connection_refused") {
		t.Fatalf("the refusal should be explicit, got %s", body)
	}

	if err := setAdminPassword("correct horse battery"); err != nil {
		t.Fatal(err)
	}
	response = postHTTPRelaySession(t, testServer)
	response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 once the password is set, got %d", response.StatusCode)
	}
}

func TestBlockClientOnlyAffectsItsChannel(t *testing.T) {
	resetAdmissionState(t)
	_, testServer := startHTTPRelayTestServer(t)

	first := joinedClient(t, testServer, "alpha")
	second := joinedClient(t, testServer, "beta")
	ip := first.GetIP()
	if second.GetIP() != ip {
		t.Fatalf("both test clients should share the loopback address, got %s and %s", ip, second.GetIP())
	}

	entry, blocked, err := blockClientByID(first.GetID())
	if err != nil {
		t.Fatal(err)
	}
	if !blocked || entry.IP != ip || entry.Channel != "alpha" || entry.ClientID != first.GetID() {
		t.Fatalf("unexpected block entry: %+v blocked=%v", entry, blocked)
	}
	waitDisconnected(t, first)
	if !FindClient(second) {
		t.Fatal("the other connection from the same address must stay connected")
	}

	if stillConnected(t, testServer, "alpha") {
		t.Fatal("the address must no longer be able to join the blocked channel")
	}
	if !stillConnected(t, testServer, "beta") {
		t.Fatal("the address must still be able to join another channel")
	}
	if !stillConnected(t, testServer, "gamma") {
		t.Fatal("the address must still be able to join a new channel")
	}

	if !unblock(ip, "alpha") {
		t.Fatal("unblocking should report a change")
	}
	if unblock(ip, "alpha") {
		t.Fatal("unblocking twice should report no change")
	}
	if !stillConnected(t, testServer, "alpha") {
		t.Fatal("the address should be accepted again on the channel")
	}

	if _, _, err := blockClientByID(999999); err != errClientNotFound {
		t.Fatalf("blocking an unknown client should fail with errClientNotFound, got %v", err)
	}
}

func TestBlockClientWithoutChannelOnlyCuts(t *testing.T) {
	resetAdmissionState(t)
	_, testServer := startHTTPRelayTestServer(t)
	createHTTPRelaySession(t, testServer)
	list, _ := snapshotClients()
	if len(list) != 1 {
		t.Fatalf("expected one client, got %d", len(list))
	}
	target := list[0]

	entry, blocked, err := blockClientByID(target.GetID())
	if err != nil {
		t.Fatal(err)
	}
	if blocked || entry.Channel != "" {
		t.Fatalf("a client without a channel must only be cut, got %+v blocked=%v", entry, blocked)
	}
	waitDisconnected(t, target)
	if len(blockedClients()) != 0 {
		t.Fatal("nothing should have been recorded")
	}
	if !stillConnected(t, testServer, "any") {
		t.Fatal("the address must still be free to join")
	}
}

func TestAdminBlockRoutesRequireSessionAndSameOrigin(t *testing.T) {
	resetAdmissionState(t)
	_, testServer := startHTTPRelayTestServer(t)
	target := joinedClient(t, testServer, "alpha")
	id := target.GetID()
	ip := target.GetIP()

	token, err := createAdminSession()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { deleteAdminSession(token) })

	call := func(handler http.HandlerFunc, form url.Values, withCookie bool, origin string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "https://relay.example/admin/clients/x", strings.NewReader(form.Encode()))
		request.Host = "relay.example"
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if withCookie {
			request.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: token})
		}
		if origin != "" {
			request.Header.Set("Origin", origin)
		}
		recorder := httptest.NewRecorder()
		handler(recorder, request)
		return recorder
	}

	if rec := call(handleAdminBlockClient, url.Values{"id": {strconv.Itoa(id)}}, false, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("without a session the block route must answer 401, got %d", rec.Code)
	}
	if rec := call(handleAdminBlockClient, url.Values{"id": {strconv.Itoa(id)}}, true, "https://evil.example"); rec.Code != http.StatusForbidden {
		t.Fatalf("a foreign origin must be refused with 403, got %d", rec.Code)
	}
	if isBlocked(ip, "alpha") {
		t.Fatal("refused requests must not block anything")
	}
	if rec := call(handleAdminBlockClient, url.Values{"id": {"abc"}}, true, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("an invalid identifier must answer 400, got %d", rec.Code)
	}
	rec := call(handleAdminBlockClient, url.Values{"id": {strconv.Itoa(id)}}, true, "https://relay.example")
	if rec.Code != http.StatusOK {
		t.Fatalf("a valid block request must answer 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"blocked":true`) || !strings.Contains(rec.Body.String(), `"channel":"alpha"`) {
		t.Fatalf("the answer must name the blocked channel, got %s", rec.Body.String())
	}
	if !isBlocked(ip, "alpha") {
		t.Fatal("the address should be blocked on the channel after the request")
	}
	waitDisconnected(t, target)
	if rec := call(handleAdminBlockClient, url.Values{"id": {strconv.Itoa(id)}}, true, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("blocking a gone connection must answer 404, got %d", rec.Code)
	}

	if rec := call(handleAdminUnblockClient, url.Values{"ip": {ip}, "channel": {"alpha"}}, false, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("without a session the unblock route must answer 401, got %d", rec.Code)
	}
	if rec := call(handleAdminUnblockClient, url.Values{"ip": {ip}}, true, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("unblocking without a channel must answer 400, got %d", rec.Code)
	}
	if rec := call(handleAdminUnblockClient, url.Values{"ip": {ip}, "channel": {"alpha"}}, true, ""); rec.Code != http.StatusOK {
		t.Fatalf("a valid unblock request must answer 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if isBlocked(ip, "alpha") {
		t.Fatal("the address should be allowed again")
	}
	if rec := call(handleAdminUnblockClient, url.Values{"ip": {ip}, "channel": {"alpha"}}, true, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unblocking an unblocked address must answer 404, got %d", rec.Code)
	}

	get := httptest.NewRequest(http.MethodGet, "https://relay.example/admin/clients/block", nil)
	get.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: token})
	recorder := httptest.NewRecorder()
	handleAdminBlockClient(recorder, get)
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET must answer 405, got %d", recorder.Code)
	}
}

func TestBlockListPersistsAcrossRestarts(t *testing.T) {
	resetAdmissionState(t)
	path := filepath.Join(t.TempDir(), "admin-history.db")
	store, err := openAdminHistory(path)
	if err != nil {
		t.Fatal(err)
	}
	adminHistoryState.Lock()
	previous := adminHistoryState.store
	adminHistoryState.store = store
	adminHistoryState.Unlock()
	t.Cleanup(func() {
		adminHistoryState.Lock()
		adminHistoryState.store = previous
		adminHistoryState.Unlock()
		store.db.Close()
	})

	blockedAt := time.Date(2026, time.October, 24, 9, 30, 0, 0, time.UTC)
	storeBlockedClient(blockedClient{IP: "198.51.100.7", Channel: "demo", ClientID: 42, BlockedAt: blockedAt})
	storeBlockedClient(blockedClient{IP: "198.51.100.7", Channel: "other", ClientID: 43, BlockedAt: blockedAt.Add(time.Minute)})

	store.db.Close()
	reopened, err := openAdminHistory(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.db.Close() })
	loaded, err := reopened.loadBlockedClients()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 2 {
		t.Fatalf("expected two stored blocks for the same address, got %d", len(loaded))
	}
	replaceBlockList(loaded)
	list := blockedClients()
	if list[0].Channel != "other" || list[1].Channel != "demo" {
		t.Fatalf("blocks should be listed most recent first, got %+v", list)
	}
	if list[1].ClientID != 42 || !list[1].BlockedAt.Equal(blockedAt) {
		t.Fatalf("stored details were lost: %+v", list[1])
	}

	adminHistoryState.Lock()
	adminHistoryState.store = reopened
	adminHistoryState.Unlock()
	if !unblock("198.51.100.7", "demo") {
		t.Fatal("the loaded block should be removable")
	}
	remaining, err := reopened.loadBlockedClients()
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 || remaining[0].Channel != "other" {
		t.Fatalf("only the unblocked channel should be gone from storage, got %+v", remaining)
	}
}

func TestValidateAdminConfiguration(t *testing.T) {
	if err := validateAdminConfiguration(false, 1); err == nil {
		t.Fatal("disabling the dashboard must be refused")
	}
	if err := validateAdminConfiguration(true, 0); err == nil {
		t.Fatal("the dashboard without a WebSocket listener must be refused")
	}
	if err := validateAdminConfiguration(true, 1); err != nil {
		t.Fatalf("a dashboard with a listener must be accepted, got %v", err)
	}
}

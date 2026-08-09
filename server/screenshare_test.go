package server

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestFilterCapabilitiesKeepsKnownValues(t *testing.T) {
	got := filterCapabilities([]string{CapScreenShare, CapInputControl})
	if len(got) != 2 || got[0] != CapScreenShare || got[1] != CapInputControl {
		t.Fatalf("expected both known capabilities, got %v", got)
	}
}

func TestFilterCapabilitiesRejectsUnknownAndDuplicates(t *testing.T) {
	got := filterCapabilities([]string{"nope", CapScreenShare, CapScreenShare, strings.Repeat("x", 4096)})
	if len(got) != 1 || got[0] != CapScreenShare {
		t.Fatalf("expected only the known capability once, got %v", got)
	}
	if filterCapabilities(nil) != nil {
		t.Fatal("expected nil for an empty capability list")
	}
}

func TestFilterCapabilitiesIsBounded(t *testing.T) {
	input := make([]string, 0, 100)
	for i := 0; i < 100; i++ {
		input = append(input, CapScreenShare, CapInputControl)
	}
	if got := filterCapabilities(input); len(got) > maxCapabilities {
		t.Fatalf("expected at most %d capabilities, got %d", maxCapabilities, len(got))
	}
}

func TestTurnCredentialsFormat(t *testing.T) {
	now := time.Unix(1700000000, 0)
	username, credential, ttl := turnCredentials(42, "secret", 600, now)
	if ttl != 600 {
		t.Fatalf("expected a ttl of 600, got %d", ttl)
	}
	expected := strconv.FormatInt(now.Add(600*time.Second).Unix(), 10) + ":42"
	if username != expected {
		t.Fatalf("expected username %q, got %q", expected, username)
	}
	if credential == "" {
		t.Fatal("expected a non empty credential")
	}
	_, other, _ := turnCredentials(42, "another secret", 600, now)
	if other == credential {
		t.Fatal("expected the credential to depend on the shared secret")
	}
}

func TestTurnCredentialsFallBackToDefaultTtl(t *testing.T) {
	if _, _, ttl := turnCredentials(1, "secret", 0, time.Now()); ttl != DEFAULT_TURN_TTL {
		t.Fatalf("expected the default ttl, got %d", ttl)
	}
}

func TestTurnUrlValidation(t *testing.T) {
	valid := []string{"stun:turn.example.com:3478", "turns:turn.example.com:5349?transport=tcp"}
	for _, v := range valid {
		if err := turn_url_valid(v); err != nil {
			t.Fatalf("expected %q to be valid, got %v", v, err)
		}
	}
	invalid := []string{"", "turn.example.com:3478", "https://turn.example.com", "turn:"}
	for _, v := range invalid {
		if err := turn_url_valid(v); err == nil {
			t.Fatalf("expected %q to be rejected", v)
		}
	}
}

func TestTurnNeedsSecret(t *testing.T) {
	if turn_needs_secret(StringList{"stun:turn.example.com:3478"}) {
		t.Fatal("a stun only configuration needs no secret")
	}
	if !turn_needs_secret(StringList{"stun:turn.example.com:3478", "turn:turn.example.com:3478"}) {
		t.Fatal("a turn relay requires a secret")
	}
}

func TestIceServersSplitsStunAndTurn(t *testing.T) {
	oldUrls, oldSecret, oldTtl := turnUrls, turnSecret, turnTtl
	defer func() { turnUrls, turnSecret, turnTtl = oldUrls, oldSecret, oldTtl }()

	turnUrls = StringList{"stun:turn.example.com:3478", "turn:turn.example.com:3478"}
	turnSecret = "secret"
	turnTtl = 300

	servers, ttl := iceServers(7)
	if len(servers) != 2 {
		t.Fatalf("expected two ice server entries, got %d", len(servers))
	}
	if servers[0].Username != "" || servers[0].Credential != "" {
		t.Fatal("the stun entry must carry no credentials")
	}
	if servers[1].Username == "" || servers[1].Credential == "" {
		t.Fatal("the turn entry must carry credentials")
	}
	if ttl != 300 {
		t.Fatalf("expected a ttl of 300, got %d", ttl)
	}
}

func TestIceServersWithoutConfiguration(t *testing.T) {
	oldUrls := turnUrls
	defer func() { turnUrls = oldUrls }()

	turnUrls = nil
	if servers, _ := iceServers(1); servers != nil {
		t.Fatalf("expected no ice servers, got %v", servers)
	}
}

func TestMaybeScreenSharePrefilter(t *testing.T) {
	if maybeScreenShare([]byte(`{"type":"speak","sequence":[1]}`)) {
		t.Fatal("a regular message must not reach the decoder")
	}
	for _, v := range []string{"webrtc_offer", "screen_share_request", "turn_credentials", "capabilities"} {
		if !maybeScreenShare([]byte(`{"type":"` + v + `"}`)) {
			t.Fatalf("expected %q to be picked up by the prefilter", v)
		}
	}
}

func TestHandleScreenShareIgnoredWhenDisabled(t *testing.T) {
	old := screenShare
	defer func() { screenShare = old }()

	screenShare = false
	if HandleScreenShare(&Client{}, &ClientChannel{}, []byte(`{"type":"webrtc_offer","target":2}`)) {
		t.Fatal("no message may be intercepted while screen sharing is disabled")
	}
}

func TestHandleScreenShareLeavesRegularMessagesAlone(t *testing.T) {
	old := screenShare
	defer func() { screenShare = old }()

	screenShare = true
	if HandleScreenShare(&Client{}, &ClientChannel{}, []byte(`{"type":"speak","sequence":["hello"]}`)) {
		t.Fatal("a regular message must be left to the normal relay")
	}
}

func TestClientDataOmitsCapabilitiesWhenAbsent(t *testing.T) {
	encoded, err := Encode(Data{
		Type:   "client_joined",
		Client: &ClientData{ID: 1, ConnectionType: connTypeSlave},
	})
	if err != nil {
		t.Fatalf("unexpected encoding error: %v", err)
	}
	if strings.Contains(string(encoded), "capabilities") {
		t.Fatalf("legacy clients must receive an unchanged payload, got %s", encoded)
	}
}

func TestDataDecodesSignalingFields(t *testing.T) {
	db, err := Decode([]byte(`{"type":"webrtc_offer","target":12,"capabilities":["screen_share/1"]}`))
	if err != nil {
		t.Fatalf("unexpected decoding error: %v", err)
	}
	if db.Target != 12 {
		t.Fatalf("expected target 12, got %d", db.Target)
	}
	if len(db.Capabilities) != 1 || db.Capabilities[0] != CapScreenShare {
		t.Fatalf("expected the screen sharing capability, got %v", db.Capabilities)
	}
}

func TestTurnCredentialsPayloadShape(t *testing.T) {
	encoded, err := Encode(Data{
		Type: "turn_credentials",
		Ttl:  3600,
		IceServers: []IceServer{
			{Urls: []string{"stun:turn.example.com:3478"}},
			{Urls: []string{"turn:turn.example.com:3478"}, Username: "1:2", Credential: "abc"},
		},
	})
	if err != nil {
		t.Fatalf("unexpected encoding error: %v", err)
	}
	decoded := make(map[string]interface{})
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unexpected decoding error: %v", err)
	}
	if _, exists := decoded["ice_servers"]; !exists {
		t.Fatalf("expected an ice_servers field, got %s", encoded)
	}
	if strings.Contains(string(encoded), "turn_secret") {
		t.Fatal("the shared secret must never be sent to clients")
	}
}

func TestClientSignalingRateLimit(t *testing.T) {
	c := &Client{}
	for i := 0; i < signalingWindowMax; i++ {
		if !c.AllowSignaling() {
			t.Fatalf("message %d was rejected before reaching the limit", i)
		}
	}
	if c.AllowSignaling() {
		t.Fatal("expected the message beyond the limit to be rejected")
	}
}

func TestScreenShareDefaultsAreOff(t *testing.T) {
	c := cfg_default()
	if c.ScreenShare {
		t.Fatal("screen sharing must be disabled by default")
	}
	if !c.IsDefault() {
		t.Fatal("a freshly built configuration must be reported as default")
	}
	c.ScreenShare = true
	if c.IsDefault() {
		t.Fatal("enabling screen sharing must make the configuration non default")
	}
}

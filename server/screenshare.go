package server

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"time"
)

// Capabilities a client may advertise through the "capabilities" message. They
// are versioned so that incompatible revisions can coexist on a server.
const (
	CapScreenShare  string = "screen_share/1"
	CapInputControl string = "input_control/1"
)

// maxCapabilities bounds the size of the capability list a client may declare.
const maxCapabilities int = 8

// Fixed window rate limit applied to signaling messages.
const (
	signalingWindow    time.Duration = 10 * time.Second
	signalingWindowMax int           = 250
)

// knownCapabilities is the allow list of capability strings the server accepts.
var knownCapabilities = map[string]bool{
	CapScreenShare:  true,
	CapInputControl: true,
}

// signalingTypes lists the message types the server routes to a single peer
// instead of broadcasting them to the whole channel.
var signalingTypes = map[string]bool{
	"screen_share_request":  true,
	"screen_share_response": true,
	"screen_share_stop":     true,
	"webrtc_offer":          true,
	"webrtc_answer":         true,
	"webrtc_candidate":      true,
}

// StringList collects a repeatable string command line parameter.
type StringList []string

func (s *StringList) String() string {
	return strings.Join(*s, "\n")
}

func (s *StringList) Set(v string) error {
	if err := turn_url_valid(v); err != nil {
		return err
	}
	*s = append(*s, v)
	return nil
}

// turn_url_valid checks that a URL handed to clients as an ICE server uses one
// of the schemes defined for STUN and TURN.
func turn_url_valid(v string) error {
	for _, scheme := range []string{"stun:", "stuns:", "turn:", "turns:"} {
		if strings.HasPrefix(v, scheme) && len(v) > len(scheme) {
			return nil
		}
	}
	return errors.New("The URL " + v + " is invalid, it must start with stun:, stuns:, turn: or turns: and name a host.")
}

// turn_needs_secret reports whether any configured URL points to a TURN relay,
// which is the only case where credentials are required.
func turn_needs_secret(urls StringList) bool {
	for _, v := range urls {
		if strings.HasPrefix(v, "turn:") || strings.HasPrefix(v, "turns:") {
			return true
		}
	}
	return false
}

// turn_secret_load reads the shared TURN secret from a file, which keeps it out
// of the process list where command line parameters are visible to other users.
func turn_secret_load(file string) error {
	data, err := file_read(fullPath(file))
	if err != nil {
		return err
	}
	secret := strings.TrimSpace(string(data))
	if secret == "" {
		return errors.New("The TURN secret file is empty.")
	}
	turnSecret = secret
	return nil
}

// ScreenShareEnabled reports whether screen sharing signaling is relayed.
func ScreenShareEnabled() bool {
	return screenShare
}

// filterCapabilities keeps only the capabilities the server knows about, which
// prevents a client from storing arbitrary data on the server and from having
// it echoed to other clients.
func filterCapabilities(capabilities []string) []string {
	if len(capabilities) == 0 {
		return nil
	}
	filtered := make([]string, 0, len(capabilities))
	for _, v := range capabilities {
		if !knownCapabilities[v] {
			continue
		}
		duplicate := false
		for _, k := range filtered {
			if k == v {
				duplicate = true
				break
			}
		}
		if duplicate {
			continue
		}
		filtered = append(filtered, v)
		if len(filtered) == maxCapabilities {
			break
		}
	}
	if len(filtered) == 0 {
		return nil
	}
	return filtered
}

// maybeScreenShare is a cheap prefilter avoiding a full JSON decode of the many
// messages that cannot possibly be screen sharing related.
func maybeScreenShare(msg []byte) bool {
	return bytes.Contains(msg, []byte("webrtc_")) ||
		bytes.Contains(msg, []byte("screen_share")) ||
		bytes.Contains(msg, []byte("turn_credentials")) ||
		bytes.Contains(msg, []byte("capabilities"))
}

// HandleScreenShare intercepts screen sharing messages sent by a client that
// already joined a channel. It reports whether the message was consumed, in
// which case the caller must not broadcast it.
func HandleScreenShare(c *Client, cc *ClientChannel, msg []byte) bool {
	if !screenShare || !maybeScreenShare(msg) {
		return false
	}
	db, err := Decode(msg)
	if err != nil {
		return false
	}
	switch {
	case db.Type == "capabilities":
		c.SetCapabilities(filterCapabilities(db.Capabilities))
		return true
	case db.Type == "turn_credentials":
		sendTurnCredentials(c)
		return true
	case signalingTypes[db.Type]:
		routeSignaling(c, cc, &db, msg)
		return true
	}
	return false
}

// sendSignalingError reports a failure to the sender without disclosing whether
// a given user id exists on the server.
func sendSignalingError(c *Client, reason string) {
	enc, err := Encode(Data{
		Type:  "error",
		Error: reason,
	})
	if err != nil {
		Log(LOG_DEBUG, "JSON encoding error for client "+strconv.Itoa(c.GetID()))
		return
	}
	c.Send(enc)
}

// routeSignaling delivers a signaling message to the single peer it targets,
// after checking that both ends are allowed to take part in the exchange.
//
// Broadcasting is deliberately avoided here. The regular relay sends every
// message from a slave to all masters of the channel without checking their
// authorization, which would expose the shared screen to unauthorized masters.
func routeSignaling(c *Client, cc *ClientChannel, db *Data, msg []byte) {
	id := c.GetID()
	if !c.AllowSignaling() {
		Log(LOG_DEBUG, "Client "+strconv.Itoa(id)+" exceeded the signaling rate limit. Dropping message.")
		return
	}
	if !c.HasCapability(CapScreenShare) {
		sendSignalingError(c, "screen_share_unsupported")
		return
	}
	if c.GetConnectionType() == connTypeMaster && !c.GetAuthorized() {
		Log(LOG_DEBUG, "Unauthorized master "+strconv.Itoa(id)+" tried to send screen sharing signaling.")
		sendSignalingError(c, "not_authorized")
		return
	}
	if db.Target <= 0 || db.Target == id {
		sendSignalingError(c, "invalid_parameters")
		return
	}
	target := cc.FindClientByID(db.Target)
	if target == nil {
		sendSignalingError(c, "target_not_found")
		return
	}
	if !target.HasCapability(CapScreenShare) {
		sendSignalingError(c, "screen_share_unsupported")
		return
	}
	if target.GetConnectionType() == connTypeMaster && !target.GetAuthorized() {
		Log(LOG_DEBUG, "Client "+strconv.Itoa(id)+" tried to send screen sharing signaling to unauthorized master "+strconv.Itoa(db.Target)+".")
		sendSignalingError(c, "target_not_found")
		return
	}
	out, err := JsonAdd(msg, "origin", id)
	if err != nil {
		Log(LOG_DEBUG, "Error adding origin to a signaling message from client "+strconv.Itoa(id)+".\r\n"+err.Error())
		return
	}
	target.Send(out)
	if db.Type == "screen_share_request" || db.Type == "screen_share_stop" {
		Log(LOG_CHANNEL, "Client "+strconv.Itoa(id)+" sent "+db.Type+" to client "+strconv.Itoa(db.Target)+" on channel "+cc.Name()+".")
	}
}

// sendTurnCredentials hands the client the ICE servers it should use, including
// short lived TURN credentials derived from the shared secret.
func sendTurnCredentials(c *Client) {
	if c.GetChannel() == nil {
		sendSignalingError(c, "invalid_parameters")
		return
	}
	if !c.HasCapability(CapScreenShare) {
		sendSignalingError(c, "screen_share_unsupported")
		return
	}
	if c.GetConnectionType() == connTypeMaster && !c.GetAuthorized() {
		sendSignalingError(c, "not_authorized")
		return
	}
	servers, ttl := iceServers(c.GetID())
	if len(servers) == 0 {
		sendSignalingError(c, "turn_unavailable")
		return
	}
	enc, err := Encode(Data{
		Type:       "turn_credentials",
		IceServers: servers,
		Ttl:        ttl,
	})
	if err != nil {
		Log(LOG_DEBUG, "JSON encoding error for client "+strconv.Itoa(c.GetID()))
		return
	}
	c.Send(enc)
}

// iceServers builds the ICE server list for a client. STUN entries need no
// credentials, TURN entries get ephemeral ones.
func iceServers(id int) ([]IceServer, int) {
	if len(turnUrls) == 0 {
		return nil, 0
	}
	var stun []string
	var relay []string
	for _, v := range turnUrls {
		if strings.HasPrefix(v, "stun:") || strings.HasPrefix(v, "stuns:") {
			stun = append(stun, v)
			continue
		}
		relay = append(relay, v)
	}
	servers := make([]IceServer, 0, 2)
	if len(stun) > 0 {
		servers = append(servers, IceServer{Urls: stun})
	}
	if len(relay) > 0 {
		if turnSecret == "" {
			return servers, 0
		}
		username, credential, ttl := turnCredentials(id, turnSecret, turnTtl, time.Now())
		servers = append(servers, IceServer{
			Urls:       relay,
			Username:   username,
			Credential: credential,
		})
		return servers, ttl
	}
	return servers, 0
}

// turnCredentials derives an ephemeral username and password following the TURN
// REST API convention used by coturn: the username carries the expiry timestamp
// and the password is the base64 encoded HMAC of that username.
//
// HMAC-SHA1 is not a free choice here, it is what the TURN REST API mandates and
// what coturn verifies. It is used as a keyed authentication code, not as a
// plain hash, and the derived credentials expire after the configured lifetime.
func turnCredentials(id int, secret string, ttl int, now time.Time) (string, string, int) {
	if ttl <= 0 {
		ttl = DEFAULT_TURN_TTL
	}
	expiry := now.Add(time.Duration(ttl) * time.Second).Unix()
	username := strconv.FormatInt(expiry, 10) + ":" + strconv.Itoa(id)
	mac := hmac.New(sha1.New, []byte(secret))
	mac.Write([]byte(username))
	credential := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	return username, credential, ttl
}

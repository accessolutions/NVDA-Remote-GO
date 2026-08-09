package server

type Data struct {
	Type              string       `json:"type"`
	Channel           string       `json:"channel,omitempty"`
	ConnectionType    string       `json:"connection_type,omitempty"`
	Version           int          `json:"version,omitempty"`
	Origin            int          `json:"origin,omitempty"`
	Key               string       `json:"key,omitempty"`
	ID                int          `json:"user_id,omitempty"`
	UserIds           []int        `json:"user_ids,omitempty"`
	Clients           []ClientData `json:"clients,omitempty"`
	Client            *ClientData  `json:"client,omitempty"`
	Error             string       `json:"error,omitempty"`
	Motd              string       `json:"motd,omitempty"`
	MotdAlwaysDisplay bool         `json:"force_display,omitempty"`
	Capabilities      []string     `json:"capabilities,omitempty"`
	Target            int          `json:"target,omitempty"`
	IceServers        []IceServer  `json:"ice_servers,omitempty"`
	Ttl               int          `json:"ttl,omitempty"`
}

type ClientData struct {
	ID             int      `json:"id"`
	ConnectionType string   `json:"connection_type"`
	Capabilities   []string `json:"capabilities,omitempty"`
}

// IceServer describes a STUN or TURN server handed to a client so it can
// establish a peer to peer media connection.
type IceServer struct {
	Urls       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}

package server

import (
	"context"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/gorilla/websocket"
)

// upgrader upgrades incoming HTTP requests to WebSocket connections. The origin
// check is permissive because NVDA Remote clients are native applications that
// do not send a meaningful Origin header; access control is handled at the
// channel/authentication layer.
var upgrader = websocket.Upgrader{
	ReadBufferSize:  16384,
	WriteBufferSize: 16384,
	CheckOrigin:     func(r *http.Request) bool { return true },
	Subprotocols:    []string{"nvdaremote/2.0"},
}

// listenWebSocket starts an HTTPS server that upgrades matching requests to
// WebSocket connections and hands them to the existing client handling logic.
func (s *Server) listenWebSocket() error {
	s.Lock()
	config := s.config
	address := s.address
	path := s.wsPath
	s.Unlock()

	listener, err := listenTLS(address, config)
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.HandleFunc(path, s.handleWebSocket)
	registerAdminRoutes(mux)
	httpServer := &http.Server{Handler: mux}

	s.Lock()
	s.resolved = listener.Addr().String()
	s.ctx, s.Stop = context.WithCancel(mctx)
	rawFallback := s.rawFallback
	s.Add(1)
	s.Unlock()

	// When the raw fallback is enabled, the listener is wrapped so that clients
	// speaking the historic newline delimited protocol are served directly
	// instead of being rejected by the HTTP server. The server context must
	// already be set at this point, since those clients are attached to it.
	var serveListener net.Listener = listener
	if rawFallback {
		serveListener = newProtocolListener(listener, s)
		Log(LOG_DEBUG, "The server at "+address+" also accepts the historic NVDA Remote protocol alongside WebSocket connections.")
	}

	// Stopping our server.
	go func() {
		<-s.ctx.Done()
		msl.Lock()
		if !stoppingServers {
			Log(LOG_DEBUG, "The WebSocket server at "+address+" has received a signal to stop.")
		}
		msl.Unlock()
		_ = httpServer.Close()
		serveListener.Close()
		s.Done()
	}()

	go func() {
		err := httpServer.Serve(serveListener)
		if err != nil && err != http.ErrServerClosed {
			msl.Lock()
			if !stoppingServers {
				Log(LOG_DEBUG, "Error serving WebSocket connections on the server at "+address+"\r\n"+err.Error()+"\r\nStopping server.")
			}
			msl.Unlock()
			s.Stop()
		}
	}()

	return nil
}

// handleWebSocket upgrades an incoming request and registers it as a client.
func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		Log(LOG_DEBUG, "Error upgrading WebSocket connection.\r\n"+err.Error())
		return
	}

	msl.Lock()
	s.Lock()
	client := &Client{
		conn:              newWSConn(conn),
		ip:                getIPFromAddr(conn.RemoteAddr()),
		port:              getPortFromAddr(conn.RemoteAddr()),
		protocol:          "websocket",
		connectedAt:       time.Now(),
		s:                 s,
		messageTerminator: s.messageTerminator,
		closed:            false,
	}
	client.ctx, client.Close = context.WithCancel(s.ctx)
	s.Add(1)
	AddClient(client)
	s.Unlock()
	msl.Unlock()
	client.listen()
}

// getIPFromAddr extracts the IP portion from a network address.
func getIPFromAddr(addr net.Addr) string {
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return ""
	}
	return host
}

// getPortFromAddr extracts the port portion from a network address.
func getPortFromAddr(addr net.Addr) int {
	_, portStr, err := net.SplitHostPort(addr.String())
	if err != nil {
		return 0
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return 0
	}
	return port
}

/*
MIT License

Copyright (c) 2016 firstrow@gmail.com

Permission is hereby granted, free of charge, to any person obtaining a copy of this software and associated documentation files (the "Software"), to deal in the Software without restriction, including without limitation the rights to use, copy, modify, merge, publish, distribute, sublicense, and/or sell copies of the Software, and to permit persons to whom the Software is furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM, OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE SOFTWARE.
*/

package server

import (
	"bufio"
	"context"
	"crypto/tls"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// TCP server.
type Server struct {
	sync.Mutex
	sync.WaitGroup
	address           string // Address to open connection: localhost:9999
	resolved          string // Address the listener is actually bound to.
	config            *tls.Config
	messageTerminator byte
	websocket         bool
	wsPath            string
	rawFallback       bool
	ctx               context.Context
	Stop              context.CancelFunc
}

var (
	mctx            context.Context
	StopServers     context.CancelFunc
	msl             sync.Mutex
	stoppingServers bool = false
)

func init() {
	mctx, StopServers = context.WithCancel(context.Background())
}

// Set message terminator.
func (s *Server) MessageTerminator(terminator byte) {
	s.Lock()
	defer s.Unlock()
	s.messageTerminator = terminator
}

// Listen starts network server.
func (s *Server) Listen() error {
	s.Lock()
	isWebsocket := s.websocket
	s.Unlock()
	if isWebsocket {
		return s.listenWebSocket()
	}
	s.Lock()
	var listener net.Listener
	var err error
	config := s.config
	address := s.address
	s.Unlock()
	if config == nil {
		listener, err = net.Listen("tcp", address)
	} else {
		listener, err = tls.Listen("tcp", address, config)
	}
	if err != nil {
		return err
	}
	s.Lock()
	s.resolved = listener.Addr().String()
	s.ctx, s.Stop = context.WithCancel(mctx)
	s.Add(1)
	s.Unlock()
	go s.accept(listener)
	return err
}

// Addr returns the address the listener is actually bound to, which differs from
// the configured address when port 0 is used. It is empty before Listen starts.
func (s *Server) Addr() string {
	s.Lock()
	defer s.Unlock()
	return s.resolved
}

func (s *Server) accept(listener net.Listener) {
	s.Lock()
	address := s.address
	s.Unlock()
	// Stopping our server.
	go func() {
		<-s.ctx.Done()
		msl.Lock()
		if !stoppingServers {
			Log(LOG_DEBUG, "The server at "+address+" has received a signal to stop.")
		}
		msl.Unlock()
		listener.Close()
		s.Done()
	}()
	for {
		conn, err := listener.Accept()
		if err != nil {
			msl.Lock()
			if !stoppingServers {
				Log(LOG_DEBUG, "Error accepting connections on the server at "+address+"\r\n"+err.Error()+"\r\nStopping server.")
			}
			msl.Unlock()
			s.Stop()
			break
		}
		client := s.newRawClient(conn, nil)
		go client.listen()
	}
}

// newRawClient registers a connection that speaks the historic newline delimited
// NVDA Remote protocol. The reader may be nil, or may be a buffered reader that
// already holds bytes read from the connection during protocol detection. The
// returned client is registered but not started, the caller must run listen.
func (s *Server) newRawClient(conn net.Conn, reader *bufio.Reader) *Client {
	msl.Lock()
	s.Lock()
	client := &Client{
		conn:              newRawConnWithReader(conn, reader, s.messageTerminator),
		ip:                getIP(conn),
		port:              getPort(conn),
		protocol:          "tcp",
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
	return client
}

// Creates new tcp server instance.
func New(address string) *Server {
	server := &Server{
		address:           address,
		messageTerminator: '\n',
	}

	return server
}

func NewWithTLSConfig(address string, config *tls.Config) *Server {
	server := New(address)
	server.config = config
	return server
}

// NewWebSocketServer creates a new server instance that accepts WebSocket
// connections over HTTPS on the given address and path. Unless the raw fallback
// is disabled, the same address also accepts the historic newline delimited
// NVDA Remote protocol, so that older clients keep working on port 443.
func NewWebSocketServer(address string, config *tls.Config, path string) *Server {
	server := NewWithTLSConfig(address, config)
	server.websocket = true
	if path == "" {
		path = "/"
	}
	server.wsPath = path
	server.rawFallback = wsRaw
	return server
}

func getIP(c net.Conn) string {
	ip, _, err := net.SplitHostPort(c.RemoteAddr().String())
	if err != nil {
		return ""
	}
	return strings.Trim(ip, "[]")
}

func getPort(c net.Conn) int {
	_, portStr, err := net.SplitHostPort(c.RemoteAddr().String())
	if err != nil {
		return 0
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return 0
	}
	return port
}

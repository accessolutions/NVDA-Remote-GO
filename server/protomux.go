package server

import (
	"bufio"
	"context"
	"crypto/tls"
	"net"
	"sync"
	"time"
)

// rawProtocolPrefix is the first byte a legacy NVDA Remote client sends once the
// TLS handshake is complete. The historic protocol is a stream of JSON objects
// terminated by a newline, so the very first decrypted byte is always an opening
// brace. A WebSocket client instead starts with an HTTP request line, which
// always begins with an uppercase ASCII letter. The two are therefore never
// ambiguous and a single byte is enough to tell them apart.
const rawProtocolPrefix byte = '{'

// protocolDetectTimeout bounds how long a connection may stay unclassified. A
// client that completes the TLS handshake and then stays silent is closed rather
// than holding a file descriptor forever. It is a variable so that tests can
// shorten it.
var protocolDetectTimeout = 10 * time.Second

// peekedConn hands a connection over to net/http after some bytes have already
// been buffered by the protocol detection. Reads go through the buffered reader
// so that nothing consumed during detection is lost, while every other method is
// forwarded to the underlying connection.
//
// Note: net/http populates Request.TLS by type asserting the connection to
// *tls.Conn. Because the connection is wrapped here, Request.TLS is nil for
// requests served through this listener. The server never relies on it.
type peekedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (p *peekedConn) Read(b []byte) (int, error) {
	return p.reader.Read(b)
}

// protocolListener wraps a TLS listener and splits incoming connections between
// the historic NVDA Remote protocol, which is handled directly, and everything
// else, which is handed to the HTTP server through Accept. This is what allows a
// single port to serve both the legacy clients and the WebSocket clients.
type protocolListener struct {
	inner net.Listener
	s     *Server
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
	mu    sync.Mutex
	err   error
}

// newProtocolListener starts the acceptance loop of a multiplexed listener. The
// server must already have its context set, since raw clients are attached to it.
func newProtocolListener(inner net.Listener, s *Server) *protocolListener {
	p := &protocolListener{
		inner: inner,
		s:     s,
		conns: make(chan net.Conn),
		done:  make(chan struct{}),
	}
	go p.run()
	return p
}

// run accepts connections from the underlying listener and classifies each one
// in its own goroutine, so that a slow client never delays the others.
func (p *protocolListener) run() {
	for {
		conn, err := p.inner.Accept()
		if err != nil {
			p.fail(err)
			return
		}
		go p.classify(conn)
	}
}

// fail records the terminal error of the underlying listener and releases every
// caller blocked in Accept.
func (p *protocolListener) fail(err error) {
	p.mu.Lock()
	if p.err == nil {
		p.err = err
	}
	p.mu.Unlock()
	p.once.Do(func() { close(p.done) })
}

// classify determines which protocol a connection speaks and routes it.
func (p *protocolListener) classify(conn net.Conn) {
	deadline := time.Now().Add(protocolDetectTimeout)
	if err := conn.SetDeadline(deadline); err != nil {
		p.abort(conn, "unable to set the protocol detection deadline", err)
		return
	}
	if tc, ok := conn.(*tls.Conn); ok {
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		err := tc.HandshakeContext(ctx)
		cancel()
		if err != nil {
			p.abort(conn, "the TLS handshake failed", err)
			return
		}
	}
	reader := bufio.NewReader(conn)
	first, err := reader.Peek(1)
	if err != nil {
		p.abort(conn, "no data was received before the protocol detection deadline", err)
		return
	}
	// The detection deadline must be cleared, otherwise every connection would be
	// torn down a few seconds after it is established.
	if err := conn.SetDeadline(time.Time{}); err != nil {
		p.abort(conn, "unable to clear the protocol detection deadline", err)
		return
	}
	if first[0] == rawProtocolPrefix {
		client := p.s.newRawClient(conn, reader)
		go client.listen()
		return
	}
	select {
	case p.conns <- &peekedConn{Conn: conn, reader: reader}:
	case <-p.done:
		conn.Close()
	}
}

// abort closes a connection that could not be classified and reports why.
func (p *protocolListener) abort(conn net.Conn, reason string, err error) {
	addr := ""
	if remote := conn.RemoteAddr(); remote != nil {
		addr = remote.String()
	}
	conn.Close()
	msl.Lock()
	if !stoppingServers {
		Log(LOG_DEBUG, "Closing the connection from "+addr+" on the multiplexed listener at "+p.Addr().String()+", "+reason+".\r\n"+err.Error())
	}
	msl.Unlock()
}

// Accept returns the next connection that is not speaking the historic protocol.
func (p *protocolListener) Accept() (net.Conn, error) {
	select {
	case conn := <-p.conns:
		return conn, nil
	case <-p.done:
		p.mu.Lock()
		err := p.err
		p.mu.Unlock()
		if err == nil {
			err = net.ErrClosed
		}
		return nil, err
	}
}

// Close stops the multiplexer and the underlying listener.
func (p *protocolListener) Close() error {
	p.once.Do(func() { close(p.done) })
	return p.inner.Close()
}

// Addr returns the address of the underlying listener.
func (p *protocolListener) Addr() net.Addr {
	return p.inner.Addr()
}

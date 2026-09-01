package server

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"time"
)

// This file lets a single TLS port serve more than one service, by looking at
// the server name a client asks for before the TLS handshake takes place.
//
// The motivation is corporate networks. They routinely block everything but
// port 443, which prevents the TURN relay used by screen sharing from being
// reached on its own ports. Publishing the relay on 443 next to the NVDA Remote
// server means both have to share that port, and the only thing available to
// tell them apart at that stage is the Server Name Indication extension of the
// TLS ClientHello.
//
// The protocol detection in protomux.go cannot be used for this, because it
// runs after the handshake has completed, at which point the server name has
// already been consumed and the wrong certificate may have been presented. The
// routing implemented here therefore sits below the TLS listener rather than
// above it, and forwards matching connections untouched, so that the target
// service performs its own handshake with its own certificate.
//
// Connections that match no rule, including connections that send no server
// name at all, are handed to the TLS listener exactly as before. Older clients
// are unaffected.

// sniDetectTimeout bounds how long a connection may take to send its
// ClientHello. A client that connects and then stays silent is closed rather
// than holding a file descriptor forever. It is a variable so that tests can
// shorten it.
var sniDetectTimeout = 10 * time.Second

// sniPeekSize is the size of the buffer used to read the ClientHello without
// consuming it. A ClientHello is normally well under two kilobytes, but post
// quantum key shares have made them noticeably larger, so the buffer is
// generous while still bounding how much an unauthenticated peer can make the
// server hold in memory.
const sniPeekSize = 20480

// errSNIHelloParsed aborts the throwaway handshake used to parse a ClientHello
// once the server name has been read. It never reaches a client.
var errSNIHelloParsed = errors.New("the ClientHello has been parsed")

// SNIRoute forwards the connections asking for a given TLS server name to
// another TCP endpoint.
type SNIRoute struct {
	Name   string
	Target string
}

// SNIRouteList collects a repeatable name=host:port command line parameter.
type SNIRouteList []SNIRoute

func (l *SNIRouteList) String() string {
	parts := make([]string, 0, len(*l))
	for _, r := range *l {
		parts = append(parts, r.Name+"="+r.Target)
	}
	return strings.Join(parts, " ")
}

// Set parses and validates one routing rule. Rejecting malformed rules at
// startup avoids a server that silently listens without routing anything.
func (l *SNIRouteList) Set(v string) error {
	name, target, found := strings.Cut(v, "=")
	if !found {
		return errors.New("a TLS routing rule must be written as name=host:port, such as turn.example.com=127.0.0.1:5349")
	}
	name = strings.ToLower(strings.TrimSpace(name))
	target = strings.TrimSpace(target)
	if name == "" {
		return errors.New("a TLS routing rule must declare the server name to match")
	}
	if strings.ContainsAny(name, " \t/") {
		return errors.New("the server name of a TLS routing rule must be a host name, such as turn.example.com")
	}
	host, port, err := net.SplitHostPort(target)
	if err != nil || host == "" || port == "" {
		return errors.New("the target of a TLS routing rule must be written as host:port, such as 127.0.0.1:5349")
	}
	for _, r := range *l {
		if r.Name == name {
			return errors.New("the server name " + name + " is already routed to " + r.Target)
		}
	}
	*l = append(*l, SNIRoute{Name: name, Target: target})
	return nil
}

// target returns where a server name must be forwarded, if anywhere. The
// comparison is case insensitive and ignores a trailing dot, since both are
// valid ways of writing the same name.
func (l SNIRouteList) target(name string) string {
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	if name == "" {
		return ""
	}
	for _, r := range l {
		if r.Name == name {
			return r.Target
		}
	}
	return ""
}

// helloReader feeds the standard library handshake parser from an already
// buffered ClientHello. Writes are discarded, so the handshake stops as soon as
// the hello has been read, which is all that is needed here. Parsing is
// delegated to crypto/tls rather than reimplemented, so that malformed or
// hostile input is handled by code that is already hardened for it.
type helloReader struct {
	reader io.Reader
}

func (h helloReader) Read(p []byte) (int, error)         { return h.reader.Read(p) }
func (h helloReader) Write(p []byte) (int, error)        { return 0, io.ErrClosedPipe }
func (h helloReader) Close() error                       { return nil }
func (h helloReader) LocalAddr() net.Addr                { return nil }
func (h helloReader) RemoteAddr() net.Addr               { return nil }
func (h helloReader) SetDeadline(t time.Time) error      { return nil }
func (h helloReader) SetReadDeadline(t time.Time) error  { return nil }
func (h helloReader) SetWriteDeadline(t time.Time) error { return nil }

// peekServerName reads the server name of the ClientHello without consuming it,
// so that the connection can still be handed over untouched. An empty name is
// returned whenever the name cannot be determined, which is not an error: the
// connection then simply follows the default route.
func peekServerName(reader *bufio.Reader) string {
	// A ClientHello is a handshake record, whose five byte header carries the
	// content type, the version and the length of what follows.
	header, err := reader.Peek(5)
	if err != nil || header[0] != 0x16 {
		return ""
	}
	length := int(header[3])<<8 | int(header[4])
	if length <= 0 || 5+length > sniPeekSize {
		return ""
	}
	raw, err := reader.Peek(5 + length)
	if err != nil {
		return ""
	}
	name := ""
	_ = tls.Server(helloReader{reader: bytes.NewReader(raw)}, &tls.Config{
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			name = hello.ServerName
			return nil, errSNIHelloParsed
		},
	}).Handshake()
	return name
}

// sniListener is a net.Listener that diverts the connections matching a routing
// rule and returns all the others to its caller. It is meant to be wrapped in a
// TLS listener, so that the connections it returns are handled exactly as they
// were before any routing existed.
type sniListener struct {
	inner  net.Listener
	routes SNIRouteList
	conns  chan net.Conn
	done   chan struct{}
	once   sync.Once
	mu     sync.Mutex
	err    error
}

// newSNIListener starts the acceptance loop of a routing listener.
func newSNIListener(inner net.Listener, routes SNIRouteList) *sniListener {
	l := &sniListener{
		inner:  inner,
		routes: routes,
		conns:  make(chan net.Conn),
		done:   make(chan struct{}),
	}
	go l.run()
	return l
}

// run accepts connections and inspects each one in its own goroutine, so that a
// slow client never delays the others.
func (l *sniListener) run() {
	for {
		conn, err := l.inner.Accept()
		if err != nil {
			l.fail(err)
			return
		}
		go l.route(conn)
	}
}

// fail records the terminal error of the underlying listener and releases every
// caller blocked in Accept.
func (l *sniListener) fail(err error) {
	l.mu.Lock()
	if l.err == nil {
		l.err = err
	}
	l.mu.Unlock()
	l.once.Do(func() { close(l.done) })
}

// route decides whether a connection must be forwarded elsewhere.
func (l *sniListener) route(conn net.Conn) {
	if err := conn.SetDeadline(time.Now().Add(sniDetectTimeout)); err != nil {
		conn.Close()
		return
	}
	reader := bufio.NewReaderSize(conn, sniPeekSize)
	target := l.routes.target(peekServerName(reader))
	// The detection deadline must be cleared, otherwise every connection would
	// be torn down a few seconds after it is established.
	if err := conn.SetDeadline(time.Time{}); err != nil {
		conn.Close()
		return
	}
	if target == "" {
		select {
		case l.conns <- &peekedConn{Conn: conn, reader: reader}:
		case <-l.done:
			conn.Close()
		}
		return
	}
	l.forward(conn, reader, target)
}

// forward relays a connection to the endpoint declared for its server name. The
// bytes already buffered are replayed first, so the target service sees an
// untouched TLS stream and performs its own handshake.
func (l *sniListener) forward(conn net.Conn, reader *bufio.Reader, target string) {
	upstream, err := net.DialTimeout("tcp", target, sniDetectTimeout)
	if err != nil {
		addr := ""
		if remote := conn.RemoteAddr(); remote != nil {
			addr = remote.String()
		}
		conn.Close()
		msl.Lock()
		if !stoppingServers {
			Log(LOG_DEBUG, "Closing the connection from "+addr+", the TLS routing target at "+target+" could not be reached.\r\n"+err.Error())
		}
		msl.Unlock()
		return
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(upstream, reader)
		closeWrite(upstream)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(conn, upstream)
		closeWrite(conn)
	}()
	wg.Wait()
	conn.Close()
	upstream.Close()
}

// closeWrite signals the end of the stream in one direction while leaving the
// other one open, so that a half closed connection is not cut short.
func closeWrite(c net.Conn) {
	type closeWriter interface{ CloseWrite() error }
	if cw, ok := c.(closeWriter); ok {
		_ = cw.CloseWrite()
	}
}

// Accept returns the next connection that matches no routing rule.
func (l *sniListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
	case <-l.done:
		l.mu.Lock()
		err := l.err
		l.mu.Unlock()
		if err == nil {
			err = net.ErrClosed
		}
		return nil, err
	}
}

// Close stops the router and the underlying listener.
func (l *sniListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return l.inner.Close()
}

// Addr returns the address of the underlying listener.
func (l *sniListener) Addr() net.Addr {
	return l.inner.Addr()
}

// listenTLS opens the TLS listener of a server. When routing rules are
// configured, the connections asking for a routed server name are forwarded
// before the handshake, and every other connection reaches the server exactly
// as it did before.
func listenTLS(address string, config *tls.Config) (net.Listener, error) {
	if len(sniRoutes) == 0 {
		return tls.Listen("tcp", address, config)
	}
	base, err := net.Listen("tcp", address)
	if err != nil {
		return nil, err
	}
	return tls.NewListener(newSNIListener(base, sniRoutes), config), nil
}

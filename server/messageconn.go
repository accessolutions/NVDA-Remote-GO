package server

import (
	"bufio"
	"bytes"
	"net"
	"time"

	"github.com/gorilla/websocket"
)

// MessageConn abstracts the underlying connection so that the client logic can
// work identically over a raw TLS stream (newline delimited framing) or over a
// WebSocket connection (one message per WebSocket text frame).
type MessageConn interface {
	// ReadMessage reads a single, complete protocol message. The returned bytes
	// do not include any framing (such as the newline terminator).
	ReadMessage() ([]byte, error)
	// WriteMessage writes a single, complete protocol message, adding any framing
	// required by the underlying transport.
	WriteMessage(b []byte) error
	// SetWriteDeadline sets the deadline for future WriteMessage calls.
	SetWriteDeadline(t time.Time) error
	// Close closes the underlying connection.
	Close() error
	// RemoteAddr returns the remote network address.
	RemoteAddr() net.Addr
}

// rawConn implements MessageConn over a raw (TLS) net.Conn using a single byte
// terminator to delimit messages. This preserves the historic NVDA Remote wire
// format.
type rawConn struct {
	conn       net.Conn
	reader     *bufio.Reader
	terminator byte
}

func newRawConn(conn net.Conn, terminator byte) *rawConn {
	return newRawConnWithReader(conn, nil, terminator)
}

// newRawConnWithReader builds a rawConn around a buffered reader that may
// already hold bytes read from the connection, such as the bytes consumed while
// detecting which protocol the client speaks. Passing the very same reader is
// mandatory, since recreating one would silently drop those bytes.
func newRawConnWithReader(conn net.Conn, reader *bufio.Reader, terminator byte) *rawConn {
	if reader == nil {
		reader = bufio.NewReader(conn)
	}
	return &rawConn{
		conn:       conn,
		reader:     reader,
		terminator: terminator,
	}
}

func (r *rawConn) ReadMessage() ([]byte, error) {
	message, err := r.reader.ReadBytes(r.terminator)
	if err != nil {
		return nil, err
	}
	return bytes.TrimRight(message, string(r.terminator)), nil
}

func (r *rawConn) WriteMessage(b []byte) error {
	num, err := r.conn.Write(append(b, r.terminator))
	if err != nil {
		return err
	}
	if num < len(b)+1 {
		return net.ErrClosed
	}
	return nil
}

func (r *rawConn) SetWriteDeadline(t time.Time) error {
	return r.conn.SetWriteDeadline(t)
}

func (r *rawConn) Close() error {
	return r.conn.Close()
}

func (r *rawConn) RemoteAddr() net.Addr {
	return r.conn.RemoteAddr()
}

// wsConn implements MessageConn over a gorilla/websocket connection. Each
// protocol message is carried in a single WebSocket text frame, so no newline
// framing is used.
type wsConn struct {
	conn *websocket.Conn
}

func newWSConn(conn *websocket.Conn) *wsConn {
	return &wsConn{conn: conn}
}

func (w *wsConn) ReadMessage() ([]byte, error) {
	_, data, err := w.conn.ReadMessage()
	if err != nil {
		return nil, err
	}
	return bytes.TrimRight(data, "\n"), nil
}

func (w *wsConn) WriteMessage(b []byte) error {
	return w.conn.WriteMessage(websocket.TextMessage, b)
}

func (w *wsConn) SetWriteDeadline(t time.Time) error {
	return w.conn.SetWriteDeadline(t)
}

func (w *wsConn) Close() error {
	return w.conn.Close()
}

func (w *wsConn) RemoteAddr() net.Addr {
	return w.conn.RemoteAddr()
}

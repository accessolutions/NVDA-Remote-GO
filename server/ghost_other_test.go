//go:build !linux

package server

import (
	"net"
	"testing"
)

// checkDeadPeerTimeout only has something to read back on Linux; elsewhere the
// test settles for the listener having handed out a plain TCP connection.
func checkDeadPeerTimeout(t *testing.T, conn *net.TCPConn) {
	t.Helper()
	if err := setDeadPeerTimeout(conn, deadPeerTimeout); err != nil {
		t.Fatalf("the portable no-op must not fail: %v", err)
	}
}

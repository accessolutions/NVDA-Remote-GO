//go:build !linux

package server

import (
	"net"
	"time"
)

// setDeadPeerTimeout has no portable equivalent outside Linux: the keep-alive
// probes remain the only early detection of a vanished peer there.
func setDeadPeerTimeout(conn *net.TCPConn, timeout time.Duration) error {
	return nil
}

//go:build linux

package server

import (
	"net"
	"syscall"
	"time"
)

// tcpUserTimeout is TCP_USER_TIMEOUT from <linux/tcp.h>, which the syscall
// package does not name.
const tcpUserTimeout = 0x12

// setDeadPeerTimeout bounds the time the kernel keeps retransmitting
// unacknowledged data before it declares the connection dead.
func setDeadPeerTimeout(conn *net.TCPConn, timeout time.Duration) error {
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var setErr error
	if err := raw.Control(func(fd uintptr) {
		setErr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, tcpUserTimeout, int(timeout.Milliseconds()))
	}); err != nil {
		return err
	}
	return setErr
}

// deadPeerTimeoutOf reads the timeout back, for the tests.
func deadPeerTimeoutOf(conn *net.TCPConn) (time.Duration, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var value int
	var getErr error
	if err := raw.Control(func(fd uintptr) {
		value, getErr = syscall.GetsockoptInt(int(fd), syscall.IPPROTO_TCP, tcpUserTimeout)
	}); err != nil {
		return 0, err
	}
	return time.Duration(value) * time.Millisecond, getErr
}

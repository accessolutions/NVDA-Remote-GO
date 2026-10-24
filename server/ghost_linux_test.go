//go:build linux

package server

import (
	"net"
	"syscall"
	"testing"
)

// checkDeadPeerTimeout verifies that the kernel holds the settings applied to an
// accepted connection: the user timeout and the keep-alive probes.
func checkDeadPeerTimeout(t *testing.T, conn *net.TCPConn) {
	t.Helper()
	got, err := deadPeerTimeoutOf(conn)
	if err != nil {
		t.Fatal(err)
	}
	if got != deadPeerTimeout {
		t.Fatalf("TCP_USER_TIMEOUT = %v, want %v", got, deadPeerTimeout)
	}
	raw, err := conn.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var keepAlive, idle, interval, count int
	var getErr error
	if err := raw.Control(func(fd uintptr) {
		read := func(level, option int) int {
			value, err := syscall.GetsockoptInt(int(fd), level, option)
			if err != nil && getErr == nil {
				getErr = err
			}
			return value
		}
		keepAlive = read(syscall.SOL_SOCKET, syscall.SO_KEEPALIVE)
		idle = read(syscall.IPPROTO_TCP, syscall.TCP_KEEPIDLE)
		interval = read(syscall.IPPROTO_TCP, syscall.TCP_KEEPINTVL)
		count = read(syscall.IPPROTO_TCP, syscall.TCP_KEEPCNT)
	}); err != nil {
		t.Fatal(err)
	}
	if getErr != nil {
		t.Fatal(getErr)
	}
	if keepAlive == 0 || idle != int(keepAliveIdle.Seconds()) || interval != int(keepAliveInterval.Seconds()) || count != keepAliveCount {
		t.Fatalf("keep-alive = %d idle=%ds interval=%ds count=%d, want enabled %v/%v/%d",
			keepAlive, idle, interval, count, keepAliveIdle, keepAliveInterval, keepAliveCount)
	}
}

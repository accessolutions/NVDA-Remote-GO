package server

import (
	"context"
	"net"
	"sync"
	"time"
)

// A client which vanishes without closing its connection (network cut, computer
// put to sleep, mobile link renegotiated) leaves a half-open TCP connection
// behind. Without help from the kernel, it is only noticed when a write finally
// fails, after the TCP retransmissions give up: about fifteen minutes on Linux
// with the default tcp_retries2. Meanwhile the client has long reconnected and
// the dashboard shows the same computer twice.
//
// Two settings bound that delay on every accepted connection:
//   - keep-alive probes detect an unreachable peer on an idle connection in
//     keepAliveIdle + keepAliveCount*keepAliveInterval, about 35 seconds;
//   - TCP_USER_TIMEOUT makes a write whose data is never acknowledged (the
//     periodic ping, speech, the keep-alive of an HTTPS stream) fail after
//     deadPeerTimeout instead of after the retransmissions give up.
const (
	keepAliveIdle     = 15 * time.Second
	keepAliveInterval = 5 * time.Second
	keepAliveCount    = 4
	deadPeerTimeout   = 60 * time.Second
)

// listenTCP opens a TCP listener whose accepted connections give up on a
// vanished peer within about a minute.
func listenTCP(address string) (net.Listener, error) {
	config := net.ListenConfig{
		KeepAliveConfig: net.KeepAliveConfig{
			Enable:   true,
			Idle:     keepAliveIdle,
			Interval: keepAliveInterval,
			Count:    keepAliveCount,
		},
	}
	listener, err := config.Listen(context.Background(), "tcp", address)
	if err != nil {
		return nil, err
	}
	return &tunedListener{Listener: listener}, nil
}

// tunedListener applies the dead peer timeout to every connection it accepts.
type tunedListener struct {
	net.Listener
	warnOnce sync.Once
}

func (l *tunedListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	if tcp, ok := conn.(*net.TCPConn); ok {
		if err := setDeadPeerTimeout(tcp, deadPeerTimeout); err != nil {
			l.warnOnce.Do(func() {
				Log(LOG_DEBUG, "Unable to set the dead peer timeout on accepted connections: "+err.Error())
			})
		}
	}
	return conn, nil
}

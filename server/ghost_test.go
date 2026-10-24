package server

import (
	"net"
	"testing"
	"time"
)

func TestFormatLastReceived(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		last time.Time
		want string
	}{
		{time.Time{}, "jamais"},
		{now.Add(-12 * time.Second), "il y a 12s"},
		{now.Add(-(63*time.Minute + 5*time.Second)), "il y a 1h03m05s"},
		{now.Add(2 * time.Second), "il y a 0s"},
	}
	for _, c := range cases {
		if got := formatLastReceived(c.last, now); got != c.want {
			t.Errorf("formatLastReceived(%v) = %q, want %q", c.last, got, c.want)
		}
	}
}

func TestMarkDuplicatesFlagsSameAddressKeyAndRole(t *testing.T) {
	infos := []adminClientInfo{
		{ID: 60, IP: "5.49.140.85", Channel: "christianarini", Role: "slave"},
		{ID: 61, IP: "5.49.140.85", Channel: "christianarini", Role: "slave"},
		{ID: 62, IP: "5.49.140.85", Channel: "autre", Role: "slave"},
		{ID: 63, IP: "5.49.140.85", Channel: "christianarini", Role: "master"},
		{ID: 64, IP: "5.49.140.85", Channel: "", Role: "-"},
		{ID: 65, IP: "5.49.140.85", Channel: "", Role: "-"},
	}
	markDuplicates(infos)
	want := map[int]bool{60: true, 61: true, 62: false, 63: false, 64: false, 65: false}
	for _, info := range infos {
		if info.Duplicate != want[info.ID] {
			t.Errorf("client %d: duplicate=%v, want %v", info.ID, info.Duplicate, want[info.ID])
		}
	}
}

func TestSnapshotReportsLastMessageAndDuplicates(t *testing.T) {
	resetAdmissionState(t)
	_, testServer := startHTTPRelayTestServer(t)

	first := joinedClient(t, testServer, "ghost")
	second := joinedClient(t, testServer, "ghost")
	other := joinedClient(t, testServer, "alone")
	defer first.Close()
	defer second.Close()
	defer other.Close()

	if first.GetLastReceived().IsZero() {
		t.Fatal("a client which joined has sent something: lastReceived must be set")
	}

	snapshot := buildAdminSnapshot()
	seen := 0
	for _, info := range snapshot.Clients {
		switch info.ID {
		case first.GetID(), second.GetID():
			seen++
			if !info.Duplicate {
				t.Errorf("client %d on the same address and key must be flagged as a duplicate", info.ID)
			}
			if info.LastReceived == "jamais" || info.LastReceived == "" {
				t.Errorf("client %d: unexpected last received %q", info.ID, info.LastReceived)
			}
		case other.GetID():
			seen++
			if info.Duplicate {
				t.Error("a client alone on its key must not be flagged")
			}
		}
	}
	if seen != 3 {
		t.Fatalf("expected the three test clients in the snapshot, saw %d", seen)
	}
}

func TestAcceptedConnectionsCarryTheDeadPeerSettings(t *testing.T) {
	listener, err := listenTCP("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			accepted <- nil
			return
		}
		accepted <- conn
	}()
	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	conn := <-accepted
	if conn == nil {
		t.Fatal("accept failed")
	}
	defer conn.Close()

	tcp, ok := conn.(*net.TCPConn)
	if !ok {
		t.Fatalf("accepted connection is a %T, the tuning needs a *net.TCPConn", conn)
	}
	checkDeadPeerTimeout(t, tcp)
}

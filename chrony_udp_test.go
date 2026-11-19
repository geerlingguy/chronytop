package main

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

func fakeUDP(t *testing.T, network string, steps ...exchange) string {
	t.Helper()
	ip := net.IPv4(127, 0, 0, 1)
	if network == "udp6" {
		ip = net.IPv6loopback
	}
	server, err := net.ListenUDP(network, &net.UDPAddr{IP: ip})
	if err != nil {
		if network == "udp6" {
			t.Skipf("IPv6 loopback unavailable: %v", err)
		}
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for _, step := range steps {
			buf := make([]byte, 4096)
			_ = server.SetReadDeadline(time.Now().Add(3 * time.Second))
			n, addr, err := server.ReadFromUDP(buf)
			if err != nil {
				t.Errorf("UDP fake read: %v", err)
				return
			}
			reply := wireReply(t, buf[:n], step)
			if reply == nil {
				return
			}
			replies := [][]byte{reply}
			if step.mutate != nil {
				replies = step.mutate(reply)
			}
			for _, r := range replies {
				if _, err := server.WriteToUDP(r, addr); err != nil {
					t.Error(err)
					return
				}
			}
		}
	}()
	t.Cleanup(func() { _ = server.Close(); <-done })
	return server.LocalAddr().String()
}

func TestUDPSamplingAndPersistence(t *testing.T) {
	for _, network := range []string{"udp4", "udp6"} {
		t.Run(network, func(t *testing.T) {
			tracking := make([]byte, 76)
			putIP(tracking[4:], "2001:db8::123")
			server := make([]byte, 168)
			server[7] = 100
			steps := []exchange{
				{command: 33, size: 104, kind: 5, body: tracking},
				{command: 14, size: 32, kind: 2, body: make([]byte, 4)},
				{command: 54, size: 196, kind: 25, body: server},
				{command: 68, size: 520, args: []uint32{0, 8, 100, 0}, kind: 21, body: clientPage(1, 1, 1)},
			}
			s := &chronySession{network: "udp", address: fakeUDP(t, network, append(steps, steps...)...)}
			defer s.Close()
			var first *chronySocket
			stats := Stats{}
			for i := 0; i < 2; i++ {
				stats.Sample(context.Background(), s)
				if stats.TrackingErr != nil || stats.SourcesErr != nil || stats.ServerErr != nil {
					t.Fatalf("UDP sample errors: %v, %v, %v", stats.TrackingErr, stats.SourcesErr, stats.ServerErr)
				}
				if stats.Tracking.Name != "2001:db8::123" || len(stats.Sources) != 0 || stats.Server[0] != 100 {
					t.Fatalf("bad UDP sample: %+v", stats)
				}
				clients, err := s.clients(context.Background(), 100)
				if err != nil || len(clients) != 1 || clients[0].Packets != 2100 {
					t.Fatalf("UDP clients: %+v, %v", clients, err)
				}
				if s.socket.dir != "" || s.socket.dirFD != -1 {
					t.Fatal("UDP created filesystem state")
				}
				if stats.MemTotal == 0 {
					t.Fatal("loopback UDP sampling skipped local system metrics")
				}
				if i == 0 {
					first = s.socket
				} else if s.socket != first {
					t.Fatal("UDP socket was replaced")
				}
			}
		})
	}
}

func TestUDPRejectionKeepsSession(t *testing.T) {
	denied := exchange{command: 54, size: 196, kind: 1, mutate: func(r []byte) [][]byte {
		put16(r, 8, 2)
		return [][]byte{r}
	}}
	s := &chronySession{network: "udp", address: fakeUDP(t, "udp4", denied,
		exchange{command: 33, size: 104, kind: 5, body: make([]byte, 76)})}
	defer s.Close()
	err := s.run(context.Background(), time.Second, func(c *chronySocket) error {
		_, err := c.ServerStats()
		return errors.Join(err) // Same aggregation as Stats.sampleChrony.
	})
	if err == nil || !strings.Contains(err.Error(), "cmdallow/opencommands") || strings.Contains(err.Error(), "sudo") {
		t.Fatalf("UDP access error: %v", err)
	}
	first := s.socket
	if first == nil {
		t.Fatal("valid rejection closed the socket")
	}
	if err := s.run(context.Background(), time.Second, queryTracking); err != nil {
		t.Fatal(err)
	}
	if s.socket != first {
		t.Fatal("valid rejection caused a reconnect")
	}
	if commandErrorsOnly(errors.Join(err, context.DeadlineExceeded)) {
		t.Fatal("transport failure treated as command rejection")
	}
}

func TestUDPRetryAndStaleReplies(t *testing.T) {
	var sequence uint32
	var previous []byte
	s := &chronySession{network: "udp", address: fakeUDP(t, "udp4",
		exchange{command: 33, size: 104, kind: 5, body: make([]byte, 76), mutate: func(r []byte) [][]byte {
			sequence = wire.Uint32(r[16:])
			return nil // Simulate a lost request or response.
		}},
		exchange{command: 33, size: 104, attempt: 1, kind: 5, body: make([]byte, 76), mutate: func(r []byte) [][]byte {
			if wire.Uint32(r[16:]) != sequence {
				t.Error("retry changed sequence")
			}
			previous = append([]byte(nil), r...)
			return [][]byte{r}
		}},
		exchange{command: 33, size: 104, kind: 5, body: make([]byte, 76), mutate: func(r []byte) [][]byte {
			if wire.Uint32(r[16:]) == sequence {
				t.Error("new query reused sequence")
			}
			return [][]byte{previous, r}
		}},
	)}
	defer s.Close()
	for i := 0; i < 2; i++ {
		if err := s.run(context.Background(), time.Second, queryTracking); err != nil {
			t.Fatal(err)
		}
	}
}

func TestUDPTimeoutAndRecovery(t *testing.T) {
	s := &chronySession{network: "udp", address: fakeUDP(t, "udp4",
		exchange{command: 33, size: 104, kind: 5, mutate: func(r []byte) [][]byte { return nil }},
		exchange{command: 33, size: 104, kind: 5, body: make([]byte, 76)},
	)}
	defer s.Close()
	start := time.Now()
	err := s.run(context.Background(), 40*time.Millisecond, queryTracking)
	var timeout net.Error
	if !errors.As(err, &timeout) || !timeout.Timeout() {
		t.Fatalf("want timeout, got %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("UDP retry extended the deadline")
	}
	if s.socket != nil {
		t.Fatal("timed-out socket was retained")
	}
	if err := s.run(context.Background(), time.Second, queryTracking); err != nil {
		t.Fatalf("UDP recovery: %v", err)
	}
}

func TestUDPCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &chronySession{network: "udp", address: fakeUDP(t, "udp4",
		exchange{command: 33, size: 104, kind: 5, mutate: func(r []byte) [][]byte { cancel(); return nil }})}
	defer s.Close()
	if err := s.run(ctx, time.Second, queryTracking); !errors.Is(err, context.Canceled) {
		t.Fatalf("UDP cancellation: %v", err)
	}
	if s.socket != nil {
		t.Fatal("cancelled socket was retained")
	}
}

func TestMonitorAddress(t *testing.T) {
	for _, tc := range []struct{ host, want string }{
		{"127.0.0.1", "127.0.0.1:323"}, {"127.0.0.1:4321", "127.0.0.1:4321"},
		{"ntp-server", "ntp-server:323"}, {"ntp-server:4321", "ntp-server:4321"},
		{"::1", "[::1]:323"}, {"[::1]", "[::1]:323"}, {"[::1]:4321", "[::1]:4321"},
		{"fe80::1%eth0", "[fe80::1%eth0]:323"},
	} {
		network, address, err := monitorAddress(defaultSocket, tc.host, false)
		if err != nil || network != "udp" || address != tc.want {
			t.Errorf("%s: %s %s %v", tc.host, network, address, err)
		}
	}
	for _, host := range []string{" ", ":323", "ntp-server:0", "ntp-server:65536", "ntp-server:abc", "ntp-server:", "[::1", "udp://ntp-server", "a/b"} {
		if _, _, err := monitorAddress(defaultSocket, host, false); err == nil {
			t.Errorf("accepted bad host %q", host)
		}
	}
	if _, _, err := monitorAddress(defaultSocket, "ntp-server", true); err == nil {
		t.Error("accepted conflicting transports")
	}
	if _, _, err := monitorAddress("relative.sock", "", true); err == nil {
		t.Error("accepted relative socket path")
	}
	if network, address, err := monitorAddress("/custom/chrony.sock", "", true); err != nil || network != "unixgram" || address != "/custom/chrony.sock" {
		t.Fatalf("socket override: %s %s %v", network, address, err)
	}
}

func TestFallbackHint(t *testing.T) {
	err := &chronyCommandError{command: cmdServer, status: 2, udp: true, fallback: true}
	if !strings.Contains(err.Error(), "use sudo") {
		t.Errorf("fallback error lacks sudo hint: %v", err)
	}
}

func TestIsRemote(t *testing.T) {
	for address, want := range map[string]bool{"127.0.0.1:323": false, "[::1]:323": false, "localhost:323": false,
		"192.0.2.10:323": true, "ntp.example:323": true} {
		if isRemote("udp", address) != want {
			t.Errorf("%s: want remote=%v", address, want)
		}
	}
	if isRemote("unixgram", defaultSocket) {
		t.Error("Unix socket reported as remote")
	}
}

func TestRemoteDisplay(t *testing.T) {
	a := &App{network: "udp", host: "ntp.example:323", visible: map[string]bool{}, interval: time.Second}
	for _, p := range panels {
		a.visible[p.key] = true
	}
	a.stats.ServerErr = &chronyCommandError{command: 54, status: 2, udp: true}
	before := a.settings().String()
	a.draw(160, 50)
	var text strings.Builder
	for _, cell := range a.scr.cells {
		text.WriteRune(cell.r)
	}
	for _, want := range []string{"ntp.example:323", "UDP", "Request counters unavailable", "not authorised over UDP"} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("missing %q", want)
		}
	}
	for _, unwanted := range []string{" load ", " up ", "CPU", "Memory", "Interface", "req/s  avg"} {
		if strings.Contains(text.String(), unwanted) {
			t.Errorf("remote view contains %q", unwanted)
		}
	}
	a.handleKey('6')
	a.handleKey('7')
	if a.settings().String() != before {
		t.Error("remote panel hiding changed local preferences")
	}
}

func TestUDPLive(t *testing.T) {
	host := os.Getenv("CHRONYTOP_TEST_UDP")
	if host == "" {
		t.Skip("set CHRONYTOP_TEST_UDP for a read-only UDP live test")
	}
	network, address, err := monitorAddress(defaultSocket, host, false)
	if err != nil {
		t.Fatal(err)
	}
	s := &chronySession{network: network, address: address}
	defer s.Close()
	stats := Stats{}
	var first *chronySocket
	for i := 0; i < 2; i++ {
		stats.Sample(context.Background(), s)
		if stats.TrackingErr != nil || stats.SourcesErr != nil {
			t.Fatalf("UDP reports: %v %v", stats.TrackingErr, stats.SourcesErr)
		}
		if stats.ServerErr != nil && !commandErrorsOnly(stats.ServerErr) {
			t.Fatal(stats.ServerErr)
		}
		if i == 0 {
			first = s.socket
		} else if s.socket != first {
			t.Fatal("live UDP session was not retained")
		}
	}
	clients, err := s.clients(context.Background(), 100)
	if err != nil && !commandErrorsOnly(err) {
		t.Fatal(err)
	}
	t.Logf("UDP %s: reference=%s, sources=%d, server error=%v, clients=%d, client error=%v", address, stats.Tracking.RefID, len(stats.Sources), stats.ServerErr, len(clients), err)
}

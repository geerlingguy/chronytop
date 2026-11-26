package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func queryTracking(c *chronySocket) error {
	_, err := c.Tracking()
	return err
}

func TestSessionReuseAndDeadlineRenewal(t *testing.T) {
	step := exchange{command: 33, size: 104, kind: 5, body: make([]byte, 76)}
	s := &chronySession{address: fakeChrony(t, step, step, step)}
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.run(ctx, time.Second, queryTracking); err != nil {
		t.Fatal(err)
	}
	first := s.socket
	// Cancellation of a finished operation must not close the retained socket.
	cancel()
	// Simulate an idle interval longer than the previous request's deadline.
	if err := first.conn.SetDeadline(time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := s.run(context.Background(), time.Second, queryTracking); err != nil {
			t.Fatal(err)
		}
		if s.socket != first {
			t.Fatal("healthy socket was replaced")
		}
	}
	if first.sequence != 3 {
		t.Errorf("sequence did not advance across operations: %d", first.sequence)
	}
	if _, err := os.Stat(first.replyPath); err != nil {
		t.Fatalf("reply socket was not retained: %v", err)
	}
	s.Close()
	s.Close()
	if _, err := os.Stat(first.replyPath); !os.IsNotExist(err) {
		t.Errorf("reply socket remains after shutdown: %v", err)
	}
}

func TestSessionReconnectAfterFailure(t *testing.T) {
	for _, failure := range []string{"timeout", "malformed"} {
		t.Run(failure, func(t *testing.T) {
			step := exchange{command: 33, size: 104, kind: 5, body: make([]byte, 76)}
			bad := step
			bad.mutate = func(r []byte) [][]byte {
				if failure == "timeout" {
					return nil
				}
				return [][]byte{r[:30]}
			}
			s := &chronySession{address: fakeChrony(t, bad, step)}
			defer s.Close()
			var oldPath string
			err := s.run(context.Background(), 50*time.Millisecond, func(c *chronySocket) error {
				oldPath = c.replyPath
				return queryTracking(c)
			})
			if err == nil || s.socket != nil {
				t.Fatalf("failed socket was retained: %v", err)
			}
			if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
				t.Fatalf("failed reply socket remains: %v", err)
			}
			if err := s.run(context.Background(), time.Second, queryTracking); err != nil {
				t.Fatalf("reconnect: %v", err)
			}
		})
	}
}

func TestSessionDaemonRestart(t *testing.T) {
	dir, err := os.MkdirTemp("", "ct-restart-")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "chronyd.sock")
	t.Cleanup(func() { _ = os.Remove(path); _ = os.Remove(dir) })
	// Each generation binds the same pathname but owns a different kernel socket.
	start := func() *net.UnixConn {
		server, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = server.SetReadDeadline(time.Now().Add(3 * time.Second))
			buf := make([]byte, 1024)
			n, addr, err := server.ReadFromUnix(buf)
			if err != nil || n != 104 {
				t.Errorf("restart daemon read: %d, %v", n, err)
				return
			}
			r := make([]byte, 104)
			r[0], r[1] = 6, 2
			copy(r[4:6], buf[4:6])
			put16(r, 6, 5)
			copy(r[16:20], buf[8:12])
			if _, err := server.WriteToUnix(r, addr); err != nil {
				t.Error(err)
			}
		}()
		t.Cleanup(func() { _ = server.Close(); <-done })
		return server
	}
	first := start()
	s := &chronySession{address: path}
	defer s.Close()
	if err := s.run(context.Background(), time.Second, queryTracking); err != nil {
		t.Fatal(err)
	}
	oldPath := s.socket.replyPath
	_ = first.Close()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	start()
	if err := s.run(context.Background(), 100*time.Millisecond, queryTracking); err == nil {
		t.Fatal("old socket unexpectedly survived daemon restart")
	}
	if err := s.run(context.Background(), time.Second, queryTracking); err != nil {
		t.Fatalf("did not reconnect to restarted daemon: %v", err)
	}
	if s.socket.replyPath == oldPath {
		t.Fatal("old reply socket was reused after restart")
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Errorf("old reply socket remains: %v", err)
	}
}

func TestSessionsDoNotBlockEachOther(t *testing.T) {
	entered := make(chan struct{})
	background := &chronySession{address: fakeChrony(t, exchange{command: 68, size: 520,
		args: []uint32{0, 8, 100, 0}, kind: 21, mutate: func(r []byte) [][]byte {
			close(entered)
			return nil // Client list remains blocked until cancelled.
		}})}
	sampler := &chronySession{address: fakeChrony(t, exchange{command: 33, size: 104, kind: 5, body: make([]byte, 76)})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := background.clients(ctx, 100); done <- err }()
	defer func() { cancel(); <-done; background.Close(); sampler.Close() }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("client query did not start")
	}
	if err := sampler.run(context.Background(), time.Second, queryTracking); err != nil {
		t.Fatalf("sampling blocked by client query: %v", err)
	}
}

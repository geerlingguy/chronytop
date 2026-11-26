package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The fake daemon checks the wire requests as well as exercising the real
// Unix datagram transport. No installed chronyd or root access is required.
type exchange struct {
	command uint16
	attempt uint16
	size    int
	args    []uint32
	kind    uint16
	body    []byte
	mutate  func([]byte) [][]byte
}

func wireReply(t *testing.T, request []byte, step exchange) []byte {
	t.Helper()
	if len(request) != step.size || len(request) < 20 {
		t.Errorf("request length %d, want %d", len(request), step.size)
		return nil
	}
	want := make([]byte, len(request))
	want[0], want[1] = 6, 1
	put16(want, 4, step.command)
	put16(want, 6, step.attempt)
	copy(want[8:12], request[8:12])
	for i, arg := range step.args {
		put32(want, 20+4*i, arg)
	}
	if !reflect.DeepEqual(request, want) {
		t.Errorf("unexpected request for command %d: %x", step.command, request)
		return nil
	}
	reply := make([]byte, 28+len(step.body))
	reply[0], reply[1] = 6, 2
	copy(reply[4:6], request[4:6])
	put16(reply, 6, step.kind)
	copy(reply[16:20], request[8:12])
	copy(reply[28:], step.body)
	return reply
}

func fakeChrony(t *testing.T, steps ...exchange) string {
	t.Helper()
	// Keep socket paths below Linux's 108-byte limit, even for long test names.
	dir, err := os.MkdirTemp("", "ct-")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "chronyd.sock")
	server, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for _, step := range steps {
			buf := make([]byte, 4096)
			_ = server.SetReadDeadline(time.Now().Add(3 * time.Second))
			n, addr, err := server.ReadFromUnix(buf)
			if err != nil {
				t.Errorf("fake daemon read: %v", err)
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
				if _, err := server.WriteToUnix(r, addr); err != nil {
					t.Errorf("fake daemon reply: %v", err)
					return
				}
			}
		}
	}()
	t.Cleanup(func() {
		_ = server.Close()
		<-done
		entries, _ := os.ReadDir(dir)
		if len(entries) != 1 || entries[0].Name() != "chronyd.sock" {
			t.Errorf("reply socket was not cleaned up: %v", entries)
		}
		_ = os.Remove(path)
		_ = os.Remove(dir)
	})
	return path
}

func connectTest(t *testing.T, path string) *chronySocket {
	t.Helper()
	c, err := openChrony(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	if err := c.conn.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	return c
}

func put16(b []byte, offset int, value uint16) { binary.BigEndian.PutUint16(b[offset:], value) }
func put32(b []byte, offset int, value uint32) { binary.BigEndian.PutUint32(b[offset:], value) }

func putIP(b []byte, address string) {
	ip := net.ParseIP(address)
	if v4 := ip.To4(); v4 != nil {
		copy(b, v4)
		put16(b, 16, 1)
	} else {
		copy(b, ip.To16())
		put16(b, 16, 2)
	}
}

func TestChronyFloat(t *testing.T) {
	for _, tc := range []struct {
		bits uint32
		want float64
	}{
		{0, 0}, {0x02800000, 0.5}, {0x03800000, -0.5},
		{0x00800000, 0.25}, {0xFE800000, 0.125},
		{0xFF800000, -0.125}, {0x7EFFFFFF, math.Ldexp(16777215, 38)},
		{0x81000000, -math.Ldexp(1, -65)},
	} {
		b := make([]byte, 4)
		put32(b, 0, tc.bits)
		if got := chronyFloat(b); got != tc.want {
			t.Errorf("%08x: got %g, want %g", tc.bits, got, tc.want)
		}
	}
}

func TestSocketTracking(t *testing.T) {
	b := make([]byte, 76)
	put32(b, 0, 0x0a630001)
	putIP(b[4:], "2001:db8::123")
	put16(b, 24, 2)
	put16(b, 26, 2)
	put32(b, 28, 1) // A timestamp beyond 2106 must retain its high word.
	put32(b, 32, 123)
	put32(b, 36, 500000000)
	for i, bits := range []uint32{0xFF800000, 0x00800000, 0x02800000, 0x03800000, 0, 0xFE800000, 0x00800000, 0x02800000} {
		put32(b, 40+i*4, bits)
	}
	c := connectTest(t, fakeChrony(t, exchange{command: 33, size: 104, kind: 5, body: b}))
	got, err := c.Tracking()
	want := &Tracking{RefID: "0A630001", Name: "2001:db8::123", Stratum: "2", Leap: "Delete second",
		RefTime: 4294967419.5, SysTime: -0.125, LastOffset: 0.25, RMSOffset: 0.5,
		Freq: -0.5, Skew: 0.125, RootDelay: 0.25, RootDispersion: 0.5}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("tracking = %+v, %v; want %+v", got, err, want)
	}
}

func TestSocketReferenceClock(t *testing.T) {
	b := make([]byte, 76)
	copy(b, "GPS\x00")
	put32(b, 28, 0x7fffffff)
	put32(b, 32, 1763434000)
	put16(b, 26, 3)
	c := connectTest(t, fakeChrony(t, exchange{command: 33, size: 104, kind: 5, body: b}))
	got, err := c.Tracking()
	if err != nil || got.Name != "GPS" || got.RefTime != 1763434000 || got.Leap != "Not synchronised" {
		t.Fatalf("reference tracking = %+v, %v", got, err)
	}
}

func TestSocketSources(t *testing.T) {
	n := make([]byte, 4)
	put32(n, 0, 7)
	steps := []exchange{{command: 14, size: 32, kind: 2, body: n}}
	for i := 0; i < 7; i++ {
		b := make([]byte, 48)
		putIP(b, "192.0.2.1")
		if i == 1 {
			putIP(b, "2001:db8::1")
		}
		if i == 2 {
			copy(b, "PPS\x00")
		}
		if i == 6 {
			put16(b, 16, 3) // Unresolved ID, omitted from the default view.
		}
		put16(b, 20, 0xfffe) // signed poll = -2
		put16(b, 22, 1)
		put16(b, 24, uint16(i))
		put16(b, 26, uint16(i%3))
		put16(b, 30, 255)
		put32(b, 32, 22)
		put32(b, 36, 0x02800000) // Raw measurement must not replace adjusted offset.
		put32(b, 40, 0xFF800000)
		put32(b, 44, 0xFE800000)
		steps = append(steps, exchange{command: 15, size: 76, args: []uint32{uint32(i)}, kind: 3, body: b})
	}
	c := connectTest(t, fakeChrony(t, steps...))
	got, err := c.Sources()
	if err != nil || len(got) != 6 {
		t.Fatalf("sources = %+v, %v", got, err)
	}
	for i, s := range got {
		if s.State != string("*?x~+-"[i]) || s.Mode != string("^=#"[i%3]) || s.Reach != "377" || s.Poll != -2 || s.Offset != -0.125 || s.Err != 0.125 || s.LastRx != 22 {
			t.Errorf("source %d: %+v", i, s)
		}
	}
	if got[0].Name != "192.0.2.1" || got[1].Name != "2001:db8::1" || got[2].Name != "PPS" {
		t.Errorf("source names: %+v", got)
	}
}

func TestSocketServerVersions(t *testing.T) {
	for _, tc := range []struct {
		kind        uint16
		count, size int
	}{{22, 8, 32}, {24, 11, 44}, {25, 17, 168}} {
		t.Run(fmt.Sprint(tc.kind), func(t *testing.T) {
			b := make([]byte, tc.size)
			base := uint64(100)
			if tc.kind == 25 {
				base = 1 << 40
			}
			for i := 0; i < tc.count; i++ {
				if tc.kind == 25 {
					binary.BigEndian.PutUint64(b[8*i:], base+uint64(i))
				} else {
					put32(b, 4*i, uint32(base)+uint32(i))
				}
			}
			c := connectTest(t, fakeChrony(t, exchange{command: 54, size: 196, kind: tc.kind, body: b}))
			got, err := c.ServerStats()
			want := []float64{0, 3, 2, 5, 6, 1, 4, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}[:tc.count]
			for i := range want {
				want[i] += float64(base)
			}
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("serverstats = %v, %v; want %v", got, err, want)
			}
			wantLines := 10 // includes the NTS rows; every counter is non-zero
			if tc.count == 8 {
				wantLines = 9
			}
			if lines := serverLines(&Stats{Server: got}, 80); len(lines) != wantLines {
				t.Errorf("server panel has %d lines, want %d: %v", len(lines), wantLines, lines)
			}
		})
	}
}

func clientPage(total, next, count uint32) []byte {
	b := make([]byte, 492)
	put32(b, 0, total)
	put32(b, 4, next)
	put32(b, 8, count)
	for i := 0; i < int(min(count, 8)); i++ {
		r := b[12+i*60:]
		putIP(r, fmt.Sprintf("192.0.2.%d", i+1))
		put32(r, 20, 2100)
		put32(r, 32, 1400)
		r[44] = 254 // signed interval = -2
	}
	return b
}

func TestSocketClientsPagination(t *testing.T) {
	page2 := clientPage(1000, 1000, 3)
	putIP(page2[12:], "2001:db8::5")
	page2[12+44] = 127
	put16(page2, 12+60+16, 0) // Missing entry must be skipped.
	c := connectTest(t, fakeChrony(t,
		exchange{command: 68, size: 520, args: []uint32{0, 8, 100, 0}, kind: 21, body: clientPage(1000, 500, 8)},
		exchange{command: 68, size: 520, args: []uint32{500, 8, 100, 0}, kind: 21, body: page2},
	))
	got, err := c.Clients(100)
	if err != nil || len(got) != 10 {
		t.Fatalf("clients = %+v, %v", got, err)
	}
	if got[0].Packets != 2100 || got[0].Dropped != 1400 || got[0].Interval != -2 || got[8].Name != "2001:db8::5" || got[8].Interval != unknownInterval {
		t.Errorf("decoded clients: %+v", got)
	}
}

func TestSocketBadClientPages(t *testing.T) {
	for _, tc := range []struct {
		name  string
		count uint32
	}{{"count", 9}, {"progress", 8}} {
		t.Run(tc.name, func(t *testing.T) {
			c := connectTest(t, fakeChrony(t, exchange{command: 68, size: 520,
				args: []uint32{0, 8, 0, 0}, kind: 21, body: clientPage(100, 0, tc.count)}))
			if _, err := c.Clients(0); err == nil {
				t.Fatal("invalid client page accepted")
			}
		})
	}
}

func TestSocketReplyValidation(t *testing.T) {
	for _, name := range []string{"header", "data", "version", "packet", "reserved", "kind", "status"} {
		t.Run(name, func(t *testing.T) {
			c := connectTest(t, fakeChrony(t, exchange{command: 33, size: 104, kind: 5, body: make([]byte, 76),
				mutate: func(r []byte) [][]byte {
					switch name {
					case "header":
						r = r[:27]
					case "data":
						r = r[:103]
					case "version":
						r[0] = 7
					case "packet":
						r[1] = 1
					case "reserved":
						r[2] = 1
					case "kind":
						put16(r, 6, 99)
					case "status":
						put16(r, 8, 2)
						put16(r, 6, 1)
						r = r[:28]
					}
					return [][]byte{r}
				}}))
			if _, err := c.Tracking(); err == nil {
				t.Fatal("invalid reply accepted")
			}
		})
	}
}

func TestSocketIgnoresUnrelatedReplies(t *testing.T) {
	c := connectTest(t, fakeChrony(t, exchange{command: 33, size: 104, kind: 5, body: make([]byte, 76),
		mutate: func(r []byte) [][]byte {
			sequence := append([]byte(nil), r...)
			sequence[19] ^= 1
			command := append([]byte(nil), r...)
			command[5] ^= 1
			return [][]byte{sequence, command, r}
		}}))
	if _, err := c.Tracking(); err != nil {
		t.Fatal(err)
	}
}

func TestSocketTimeoutAndCancellation(t *testing.T) {
	for _, cancelRequest := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelRequest), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			path := fakeChrony(t, exchange{command: 33, size: 104, kind: 5, mutate: func(r []byte) [][]byte {
				if cancelRequest {
					cancel()
				}
				return nil
			}})
			session := &chronySession{address: path}
			defer session.Close()
			start := time.Now()
			err := session.run(ctx, 50*time.Millisecond, func(c *chronySocket) error {
				_, err := c.Tracking()
				return err
			})
			if err == nil {
				t.Fatal("missing reply accepted")
			}
			if cancelRequest && !errors.Is(err, context.Canceled) {
				t.Errorf("cancellation error: %v", err)
			}
			if session.socket != nil {
				t.Error("failed connection was retained")
			}
			if time.Since(start) > time.Second {
				t.Fatal("request did not terminate promptly")
			}
		})
	}
}

func TestSocketReconnectAndCleanup(t *testing.T) {
	path := fakeChrony(t, exchange{command: 33, size: 104, kind: 5, body: make([]byte, 76)})
	// Simulate the command socket disappearing and coming back at the same path.
	if err := os.Rename(path, path+".offline"); err != nil {
		t.Fatal(err)
	}
	if _, err := openChrony(path); err == nil {
		t.Fatal("missing server accepted")
	}
	if err := os.Rename(path+".offline", path); err != nil {
		t.Fatal(err)
	}
	c := connectTest(t, path)
	if _, err := c.Tracking(); err != nil {
		t.Fatal(err)
	}
	c.Close()
	c.Close() // Closing twice must not remove unrelated descriptors or files.
	if _, err := os.Stat(c.replyPath); !os.IsNotExist(err) {
		t.Errorf("reply socket remains: %v", err)
	}
}

func TestSocketLive(t *testing.T) {
	path := os.Getenv("CHRONYTOP_TEST_SOCKET")
	if path == "" {
		t.Skip("set CHRONYTOP_TEST_SOCKET for a read-only live test")
	}
	c := connectTest(t, path)
	tracking, err := c.Tracking()
	if err != nil {
		t.Fatal(err)
	}
	sources, err := c.Sources()
	if err != nil {
		t.Fatal(err)
	}
	server, err := c.ServerStats()
	if err != nil {
		t.Fatal(err)
	}
	clients, err := c.Clients(0)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := json.Marshal(struct {
		Tracking *Tracking
		Sources  []Source
		Server   []float64
		Clients  []Client
	}{tracking, sources, server, clients})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("snapshot: %s", snapshot)
}

func TestSocketErrorsAreVisible(t *testing.T) {
	s := Stats{}
	session := &chronySession{address: filepath.Join(t.TempDir(), "missing.sock")}
	defer session.Close()
	s.sampleChrony(context.Background(), session)
	for _, lines := range [][]line{trackingLines(&s, 80), sourcesLines(&s, 80), serverLines(&s, 80)} {
		if len(lines) < 2 || !strings.Contains(lines[1][0].text, "connect to chronyd") {
			t.Errorf("missing socket error: %v", lines)
		}
	}
}

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFormat(t *testing.T) {
	cases := []struct{ got, want string }{
		{fmtSecs(0.0000131, true), "+13.1µs"},
		{fmtSecs(-0.0016, true), "-1.6ms"},
		{fmtSecs(0.000000844, false), "844.0ns"},
		{fmtSI(0, ""), "0"},
		{fmtSI(2.5, ""), "2.5"},
		{fmtSI(1500, "bps"), "1.5kbps"},
		{fmtBytes(1536), "1.5 KiB"},
		{fmtDur(3725), "1h 2m"},
		{fmtAgo(1016), "16m"},
		{fmtAgo(4294967295), "-"},
		{fmtLog2(-3), "0.12s"},
		{fmtLog2(10), "1024s"},
		{fmtLog2(unknownInterval), "-"},
		{fmtCount(887837), "887,837"},
		{fmtCount(-1234), "-1,234"},
		{clean("bad\x1b[2Jname"), "bad[2Jname"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("got %q, want %q", c.got, c.want)
		}
	}
}

func TestSettings(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if _, ok := loadSettings(); ok {
		t.Fatal("settings shouldn't exist yet")
	}
	want := Settings{Shown: map[string]bool{"requests": true, "tracking": true}, Interval: 2 * time.Second}
	if err := saveSettings(want); err != nil {
		t.Fatal(err)
	}
	got, ok := loadSettings()
	if !ok || got.String() != want.String() || got.Shown["server"] {
		t.Errorf("got %q, want %q", got, want)
	}

	// A symlinked settings file is ignored, and replaced (not followed) on save.
	dir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "chronytop")
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, configName)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, ok := loadSettings(); ok {
		t.Error("symlinked settings should be ignored")
	}
	if err := saveSettings(want); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(target); string(data) != "keep" {
		t.Error("save followed a symlink")
	}

	if s := parseSettings("interval = 1h\nbogus\n"); s.Interval != maxInterval || s.Shown != nil {
		t.Errorf("parse: %+v", s)
	}
}

func TestServerLinesFieldCounts(t *testing.T) {
	cases := []struct {
		server []float64
		want   int
	}{
		{[]float64{12080, 0, 4958284, 0, 0, 0, 0, 0}, 6},                    // chrony 4.0
		{[]float64{887837, 13218, 628, 0, 0, 0, 0, 0, 42, 43, 30350}, 7},    // chrony 4.3
		{[]float64{887837, 13218, 628, 0, 0, 5, 0, 100, 42, 43, 30350}, 10}, // with NTS
		{nil, 1},
	}
	for _, tc := range cases {
		st := &Stats{Server: tc.server}
		if got := len(serverLines(st, 50)); got != tc.want {
			t.Errorf("%v: got %d lines, want %d", tc.server, got, tc.want)
		}
	}
}

func TestAddServerRates(t *testing.T) {
	var s Stats
	prev := []float64{1000, 10, 0, 0, 0, 0, 0, 400}
	s.addServerRates([]float64{1100, 12, 0, 0, 0, 0, 0, 430}, prev, 2)
	if last(s.Req) != 50 || last(s.Drop) != 1 || last(s.Auth) != 15 {
		t.Errorf("req %v drop %v auth %v", s.Req, s.Drop, s.Auth)
	}
	// A chronyd restart resets the counters; skip that sample.
	s.addServerRates([]float64{5, 0, 0, 0, 0, 0, 0, 1}, prev, 1)
	if len(s.Req) != 1 || len(s.Auth) != 1 {
		t.Errorf("reset sample was recorded: %v %v", s.Req, s.Auth)
	}
}

func TestGraphStacksPart(t *testing.T) {
	var s Screen
	// Bars are right-aligned in columns 2-4, clear of the axis labels. Each is 2 rows tall:
	// all NTP, half NTS, all NTS.
	s.Reset(5, 2)
	s.Graph(0, 0, 2, 5, []float64{10, 10, 10}, []float64{0, 5, 10}, green, cyan)
	at := func(y, x int) cell { return s.cells[y*s.W+x] }
	for _, c := range []struct {
		y, x int
		want Style
	}{
		{1, 2, green}, {0, 2, green},
		{1, 3, cyan}, {0, 3, green}, // NTS fills the bottom row
		{1, 4, cyan}, {0, 4, cyan},
	} {
		if got := at(c.y, c.x).st; got != c.want {
			t.Errorf("cell %d,%d style %+v, want %+v", c.y, c.x, got, c.want)
		}
	}

	// A split inside a full cell uses a background color for the rest of the cell.
	s.Reset(5, 1)
	s.Graph(0, 0, 1, 5, []float64{8}, []float64{3}, green, cyan)
	if c := at(0, 4); c.r != '▃' || c.st != (Style{FG: cyan.FG, BG: green.FG + 10}) {
		t.Errorf("split cell = %q %+v", c.r, c.st)
	}
}

func TestRequestsHeaderNTS(t *testing.T) {
	row := func(s *Screen, y int) string {
		var b []rune
		for _, c := range s.cells[y*s.W : (y+1)*s.W] {
			b = append(b, c.r)
		}
		return string(b)
	}
	for _, tc := range []struct {
		name   string
		server []float64
		auth   []float64
		want   string // "" means no NTS figure
	}{
		{"no NTS configured", []float64{100, 0, 0, 0, 0, 0, 0, 0}, []float64{0}, ""},
		{"chrony without the counter", []float64{100, 0}, nil, ""},
		{"NTS traffic", []float64{100, 0, 0, 0, 0, 3, 0, 40}, []float64{4}, "NTS 4.0/s 40%"},
		// NTS that stopped long ago (scrolled off the graph) is hidden again...
		{"old NTS", []float64{100, 0, 0, 0, 0, 3, 0, 40}, append([]float64{4}, make([]float64, 200)...), ""},
		// ...but NTS still on screen keeps the figure, even if the latest sample has none.
		{"recent NTS", []float64{100, 0, 0, 0, 0, 3, 0, 40}, []float64{4, 0}, "NTS 0/s 0%"},
	} {
		req := make([]float64, len(tc.auth))
		for i := range req {
			req[i] = 10
		}
		if len(req) == 0 {
			req = []float64{10}
		}
		a := &App{interval: time.Second, stats: Stats{Server: tc.server, Req: req, Auth: tc.auth}}
		a.scr.Reset(120, 8)
		a.requests(0, 8, 120)
		header := row(&a.scr, 0)
		if !strings.Contains(header, "10 req/s  avg") && tc.want == "" {
			t.Errorf("%s: header %q", tc.name, header)
		}
		if tc.want != "" && !strings.Contains(header, "10 req/s  "+tc.want+"  avg") {
			t.Errorf("%s: header %q, want %q", tc.name, header, tc.want)
		}
	}
}

func TestTopOffenders(t *testing.T) {
	var clients []Client
	for i := range maxOffenders + 50 {
		clients = append(clients, Client{Name: fmt.Sprintf("192.0.2.%d", i), Dropped: float64(i % 7), Interval: i % 3})
	}
	got := topOffenders(clients)
	if len(got) != maxOffenders || got[0].Dropped != 6 || got[0].Interval != 0 {
		t.Fatalf("top offenders: %d, first %+v", len(got), got[0])
	}
	for i := 1; i < len(got); i++ {
		if got[i].Dropped > got[i-1].Dropped {
			t.Fatalf("not sorted by drops at %d: %+v", i, got[i-1:i+1])
		}
	}
}

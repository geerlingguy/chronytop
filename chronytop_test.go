package main

import (
	"os"
	"path/filepath"
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
		{[]float64{12080, 0, 4958284, 0, 0, 0, 0, 0}, 6},                 // chrony 4.0
		{[]float64{887837, 13218, 628, 0, 0, 0, 0, 0, 42, 43, 30350}, 7}, // chrony 4.3
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

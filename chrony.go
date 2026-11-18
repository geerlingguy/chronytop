package main

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Fixed absolute paths, so a modified PATH can't swap in another binary when run with sudo.
// The others are where Homebrew and source installs put chronyc on macOS.
var chronycPath = firstExisting("/usr/bin/chronyc", "/usr/local/bin/chronyc", "/opt/homebrew/bin/chronyc")

func firstExisting(paths ...string) string {
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return paths[0]
}

// chronyc runs a chronyc command and returns its CSV output as rows of fields.
func chronyc(timeout time.Duration, args ...string) ([][]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	// #nosec G204 -- fixed binary; args are constants or a formatted integer.
	cmd := exec.CommandContext(ctx, chronycPath, append([]string{"-c", "-n"}, args...)...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LC_ALL=C"}
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return parseCSV(string(out)), nil
}

func parseCSV(out string) [][]string {
	var rows [][]string
	for _, line := range strings.Split(out, "\n") {
		if line = clean(strings.TrimSpace(line)); line != "" {
			rows = append(rows, strings.Split(line, ","))
		}
	}
	return rows
}

func num(s string) float64 {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return v
}

// Tracking is the output of chronyc tracking.
type Tracking struct {
	RefID, Name, Stratum, Leap string
	RefTime, SysTime           float64
	LastOffset, RMSOffset      float64
	Freq, Skew                 float64
	RootDelay, RootDispersion  float64
}

func parseTracking(rows [][]string) *Tracking {
	if len(rows) == 0 || len(rows[0]) < 14 {
		return nil
	}
	f := rows[0]
	return &Tracking{
		RefID: f[0], Name: f[1], Stratum: f[2], Leap: f[13],
		RefTime: num(f[3]), SysTime: num(f[4]), LastOffset: num(f[5]), RMSOffset: num(f[6]),
		Freq: num(f[7]), Skew: num(f[9]), RootDelay: num(f[10]), RootDispersion: num(f[11]),
	}
}

// Source is one row of chronyc sources.
type Source struct {
	Mode, State, Name, Stratum, Reach string
	Poll                              int
	LastRx, Offset, Err               float64
}

func parseSources(rows [][]string) []Source {
	var out []Source
	for _, f := range rows {
		if len(f) < 10 {
			continue
		}
		out = append(out, Source{
			Mode: f[0], State: f[1], Name: f[2], Stratum: f[3], Reach: f[5],
			Poll: int(num(f[4])), LastRx: num(f[6]), Offset: num(f[7]), Err: num(f[9]),
		})
	}
	return out
}

// Client is one row of chronyc clients.
type Client struct {
	Name             string
	Packets, Dropped float64
	// Interval is log2 seconds between packets; unknownInterval if not known.
	Interval int
}

// chronyc prints 127 in CSV mode (and "-" otherwise) when an interval isn't known.
const unknownInterval = 127

func parseClients(rows [][]string) []Client {
	var out []Client
	for _, f := range rows {
		if len(f) < 6 {
			continue
		}
		interval := unknownInterval
		if v, err := strconv.Atoi(f[3]); err == nil {
			interval = v
		}
		out = append(out, Client{Name: f[0], Packets: num(f[1]), Dropped: num(f[2]), Interval: interval})
	}
	return out
}

func parseServerStats(rows [][]string) []float64 {
	if len(rows) == 0 {
		return nil
	}
	out := make([]float64, len(rows[0]))
	for i, v := range rows[0] {
		out[i] = num(v)
	}
	return out
}

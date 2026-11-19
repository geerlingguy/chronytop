package main

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	history = 4000
	// USER_HZ is 100 on all mainstream Linux architectures.
	clockTicks = 100
)

// Stats holds the latest samples and history.
type Stats struct {
	TrackingErr, SourcesErr, ServerErr error

	Iface      string
	Req, Drop  []float64
	Tracking   *Tracking
	Sources    []Source
	Server     []float64
	Clients    []Client
	ClientsErr error
	ClientsAt  time.Time
	Net        []float64
	NetRate    []float64
	CPU        float64
	ChronydCPU float64
	OwnCPU     float64
	MemTotal   float64
	MemUsed    float64
	ChronydRSS float64

	chronydPID string
	prev       *snapshot
}

type snapshot struct {
	at                   time.Time
	server, net          []float64
	cpuBusy, cpuTotal    float64
	chronydSecs, ownSecs float64
	chronydFound         bool
}

// Sample takes one sample of everything except the client list.
func (s *Stats) Sample(ctx context.Context, session *chronySession) {
	now := time.Now()
	s.sampleChrony(ctx, session)
	snap := &snapshot{at: now, server: s.Server}
	if !isRemote(session.network, session.address) {
		s.Net = netCounters(s.Iface)
		snap.net = s.Net
		snap.cpuBusy, snap.cpuTotal = cpuTicks()
		if s.chronydPID == "" {
			s.chronydPID = findPID("chronyd")
		}
		if secs, rss, ok := procUsage(s.chronydPID); ok {
			snap.chronydSecs, snap.chronydFound, s.ChronydRSS = secs, true, rss
		} else {
			s.chronydPID, s.ChronydRSS = "", 0
		}
		snap.ownSecs = ownCPUSeconds()
		s.MemTotal, s.MemUsed = memInfo()
	}

	if p := s.prev; p != nil {
		dt := now.Sub(p.at).Seconds()
		// Counters reset when chronyd restarts.
		if len(s.Server) > 1 && len(p.server) > 1 && s.Server[0] >= p.server[0] {
			s.Req = appendCapped(s.Req, (s.Server[0]-p.server[0])/dt)
			s.Drop = appendCapped(s.Drop, (s.Server[1]-p.server[1])/dt)
		}
		if len(s.Net) == 4 && len(p.net) == 4 {
			s.NetRate = make([]float64, 4)
			for i := range s.Net {
				s.NetRate[i] = (s.Net[i] - p.net[i]) / dt
			}
		}
		if snap.cpuTotal > p.cpuTotal {
			s.CPU = (snap.cpuBusy - p.cpuBusy) / (snap.cpuTotal - p.cpuTotal) * 100
		}
		if snap.chronydFound && p.chronydFound {
			s.ChronydCPU = (snap.chronydSecs - p.chronydSecs) / dt * 100
		}
		s.OwnCPU = (snap.ownSecs - p.ownSecs) / dt * 100
	}
	s.prev = snap
}

func appendCapped(v []float64, x float64) []float64 {
	v = append(v, x)
	if len(v) > history {
		v = v[len(v)-history:]
	}
	return v
}

func (s *Stats) sampleChrony(ctx context.Context, session *chronySession) {
	s.Tracking, s.Sources, s.Server = nil, nil, nil
	queried := false
	err := session.run(ctx, 2*time.Second, func(c *chronySocket) error {
		queried = true
		s.Tracking, s.TrackingErr = c.Tracking()
		s.Sources, s.SourcesErr = c.Sources()
		s.Server, s.ServerErr = c.ServerStats()
		return errors.Join(s.TrackingErr, s.SourcesErr, s.ServerErr)
	})
	if !queried {
		s.TrackingErr, s.SourcesErr, s.ServerErr = err, err, err
	}
}

func readFile(path string) string {
	b, err := os.ReadFile(path) // #nosec G304 -- only fixed /proc paths.
	if err != nil {
		return ""
	}
	return string(b)
}

func defaultIface() string {
	lines := strings.Split(readFile("/proc/net/route"), "\n")
	for _, line := range lines[min(1, len(lines)):] {
		if f := strings.Fields(line); len(f) > 1 && f[1] == "00000000" {
			return f[0]
		}
	}
	return "eth0"
}

// netCounters returns rx bytes, rx packets, tx bytes and tx packets.
func netCounters(iface string) []float64 {
	for _, line := range strings.Split(readFile("/proc/net/dev"), "\n") {
		name, rest, ok := strings.Cut(line, ":")
		if f := strings.Fields(rest); ok && strings.TrimSpace(name) == iface && len(f) >= 10 {
			return []float64{num(f[0]), num(f[1]), num(f[8]), num(f[9])}
		}
	}
	return nil
}

// cpuTicks returns busy and total jiffies across all CPUs.
func cpuTicks() (busy, total float64) {
	line, _, _ := strings.Cut(readFile("/proc/stat"), "\n")
	f := strings.Fields(line)
	if len(f) < 9 {
		return 0, 0
	}
	for i, v := range f[1:9] {
		total += num(v)
		if i != 3 && i != 4 { // idle and iowait
			busy += num(v)
		}
	}
	return busy, total
}

func memInfo() (total, used float64) {
	var avail float64
	for _, line := range strings.Split(readFile("/proc/meminfo"), "\n") {
		k, v, _ := strings.Cut(line, ":")
		f := strings.Fields(v)
		if len(f) == 0 {
			continue
		}
		switch k {
		case "MemTotal":
			total = num(f[0]) * 1024
		case "MemAvailable":
			avail = num(f[0]) * 1024
		}
	}
	return total, total - avail
}

func findPID(name string) string {
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err == nil &&
			strings.TrimSpace(readFile("/proc/"+e.Name()+"/comm")) == name {
			return e.Name()
		}
	}
	return ""
}

// procUsage returns CPU seconds and resident memory for a pid.
func procUsage(pid string) (secs, rss float64, ok bool) {
	if pid == "" {
		return 0, 0, false
	}
	stat := readFile("/proc/" + pid + "/stat")
	i := strings.LastIndexByte(stat, ')')
	if i < 0 {
		return 0, 0, false
	}
	f := strings.Fields(stat[i+1:])
	if len(f) < 22 {
		return 0, 0, false
	}
	return (num(f[11]) + num(f[12])) / clockTicks, num(f[21]) * float64(os.Getpagesize()), true
}

func ownCPUSeconds() float64 {
	var ru syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &ru) == nil {
		return float64(ru.Utime.Nano()+ru.Stime.Nano()) / 1e9
	}
	return 0
}

func loadAvg() string {
	f := strings.Fields(readFile("/proc/loadavg"))
	if len(f) < 3 {
		return ""
	}
	return strings.Join(f[:3], " ")
}

func uptime() float64 {
	f := strings.Fields(readFile("/proc/uptime"))
	if len(f) == 0 {
		return 0
	}
	return num(f[0])
}

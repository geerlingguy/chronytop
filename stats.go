package main

import (
	"strconv"
	"syscall"
	"time"
)

const history = 4000

// Stats holds the latest samples and history.
type Stats struct {
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
	cpu        cpuSampler
	prev       *snapshot
}

type snapshot struct {
	at                   time.Time
	server, net          []float64
	chronydSecs, ownSecs float64
	chronydFound         bool
}

// Sample takes one sample of everything except the client list.
func (s *Stats) Sample() {
	now := time.Now()
	rows, _ := chronyc(2*time.Second, "tracking")
	s.Tracking = parseTracking(rows)
	rows, _ = chronyc(2*time.Second, "sources")
	s.Sources = parseSources(rows)
	rows, _ = chronyc(2*time.Second, "serverstats")
	s.Server = parseServerStats(rows)
	s.Net = netCounters(s.Iface)

	snap := &snapshot{at: now, server: s.Server, net: s.Net}
	if pct, ok := s.cpu.percent(); ok {
		s.CPU = min(100, max(0, pct))
	}
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

// fetchClients returns clients with at least minPackets packets, filtered inside chronyd.
func fetchClients(minPackets int) ([]Client, error) {
	rows, err := chronyc(10*time.Second, "clients", "-p", strconv.Itoa(minPackets))
	if err != nil {
		return nil, err
	}
	return parseClients(rows), nil
}

// ownCPUSeconds includes the chronyc processes chronytop runs.
func ownCPUSeconds() float64 {
	var total float64
	for _, who := range []int{syscall.RUSAGE_SELF, syscall.RUSAGE_CHILDREN} {
		var ru syscall.Rusage
		if syscall.Getrusage(who, &ru) == nil {
			total += float64(ru.Utime.Nano()+ru.Stime.Nano()) / 1e9
		}
	}
	return total
}

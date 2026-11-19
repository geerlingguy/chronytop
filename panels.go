package main

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

type seg struct {
	text string
	st   Style
}

type line []seg

func kv(label, value string, st Style) line {
	return line{{fmt.Sprintf("%-18s", label), dim}, {value, st}}
}

func offsetStyle(sec float64) Style {
	switch a := math.Abs(sec); {
	case a < 1e-3:
		return green
	case a < 10e-3:
		return yellow
	}
	return red
}

// fastSlow mirrors chronyc's wording; a positive system time offset means the clock is slow.
func fastSlow(v float64, positiveIsFast bool) string {
	if (v > 0) == positiveIsFast {
		return "fast"
	}
	return "slow"
}

func meter(pct float64) []seg {
	const width = 10
	n := int(math.Round(math.Min(100, math.Max(0, pct)) / 100 * width))
	st := green
	if pct >= 85 {
		st = red
	} else if pct >= 60 {
		st = yellow
	}
	return []seg{{strings.Repeat("■", n), st}, {strings.Repeat("■", width-n) + " ", dim}}
}

// nameWidth fits the longest name (at least 15 columns) into the space left by the other columns.
func nameWidth(space int, names func(func(string) bool)) int {
	width := 0
	for n := range names {
		width = max(width, len(n))
	}
	return max(9, min(max(15, width), space))
}

func last(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	return v[len(v)-1]
}

func serverLines(st *Stats, _ int) []line {
	s := st.Server
	if len(s) < 2 {
		return unavailable("Server statistics", st.ServerErr)
	}
	drop := last(st.Drop)
	dropSt, totalSt, pct := plain, plain, 0.0
	if drop > 0 {
		dropSt = red
	}
	if s[0] > 0 {
		pct = s[1] / s[0] * 100
	}
	if s[1] > 0 {
		totalSt = yellow
	}
	lines := []line{
		kv("Requests/s", fmtSI(last(st.Req), ""), green.bold()),
		kv("Dropped/s", fmtSI(drop, ""), dropSt),
		kv("Dropped total", fmt.Sprintf("%.2f%%", pct), totalSt),
	}
	for _, f := range serverStatsFields {
		if f.index < len(s) {
			lines = append(lines, kv(f.label, fmtCount(s[f.index]), plain))
		}
	}
	return lines
}

// Counters in chronyc report order; older chrony versions provide fewer fields (4.0 has 8).
var serverStatsFields = []struct {
	index int
	label string
}{
	{0, "NTP received"}, {1, "NTP dropped"}, {4, "Clientlog dropped"}, {8, "Interleaved NTP"},
}

func offenderLines(st *Stats, w, maxRows, minPackets int) []line {
	switch {
	case st.ClientsAt.IsZero():
		return []line{{{"Loading clients…", dim}}}
	case st.ClientsErr != nil:
		return unavailable("Clients", st.ClientsErr)
	case len(st.Clients) == 0:
		return []line{{{fmt.Sprintf("No clients with ≥%d packets", minPackets), dim}}}
	}
	// Most drops first, then most frequent.
	rows := append([]Client(nil), st.Clients...)
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Dropped != rows[j].Dropped {
			return rows[i].Dropped > rows[j].Dropped
		}
		return rows[i].Interval < rows[j].Interval
	})
	rows = rows[:min(len(rows), max(1, maxRows))]
	width := nameWidth(w-32, func(yield func(string) bool) {
		for _, c := range rows {
			if !yield(c.Name) {
				return
			}
		}
	})
	lines := []line{{{fmt.Sprintf("%-*s  Packets  Dropped  Drop%%  Every", width, "Client"), dim}}}
	for _, c := range rows {
		st, pct := plain, 0.0
		if c.Dropped > 0 {
			st = red
		}
		if c.Packets > 0 {
			pct = c.Dropped / c.Packets * 100
		}
		name := c.Name
		if len(name) > width {
			name = name[:width]
		}
		nameSt := st
		if c.Dropped > 0 {
			nameSt = st.bold()
		}
		lines = append(lines, line{
			{fmt.Sprintf("%-*s  ", width, name), nameSt},
			{fmt.Sprintf("%7s  %7s  %4.0f%%  ", fmtSI(c.Packets, ""), fmtSI(c.Dropped, ""), pct), st},
			{fmt.Sprintf("%5s", fmtLog2(c.Interval)), dim},
		})
	}
	return append(lines, line{{fmt.Sprintf("%d clients with ≥%d packets, every %s",
		len(st.Clients), minPackets, clientsInterval), dim}})
}

func sourcesLines(st *Stats, w int) []line {
	if st.SourcesErr != nil {
		return unavailable("Sources", st.SourcesErr)
	}
	if len(st.Sources) == 0 {
		return []line{{{"No sources", dim}}}
	}
	width := nameWidth(w-40, func(yield func(string) bool) {
		for _, s := range st.Sources {
			if !yield(s.Name) {
				return
			}
		}
	})
	lines := []line{{{fmt.Sprintf("   %-*s %2s %5s %5s %4s %8s %7s", width, "Source",
		"St", "Poll", "Reach", "Last", "Offset", "±Err"), dim}}}
	for _, s := range st.Sources {
		stateSt := red
		switch s.State {
		case "*":
			stateSt = green.bold()
		case "+":
			stateSt = cyan
		case "-":
			stateSt = plain
		}
		reachSt := yellow
		if s.Reach == "377" {
			reachSt = green
		}
		name := s.Name
		if len(name) > width {
			name = name[:width]
		}
		row := line{
			{fmt.Sprintf("%s%s %-*s ", s.Mode, s.State, width, name), stateSt},
			{fmt.Sprintf("%2s %5s ", s.Stratum, fmtLog2(s.Poll)), plain},
			{fmt.Sprintf("%5s ", s.Reach), reachSt},
			{fmt.Sprintf("%4s ", fmtAgo(s.LastRx)), plain},
		}
		if s.LastRx >= neverSampled {
			row = append(row, seg{fmt.Sprintf("%8s %7s", "-", "-"), dim})
		} else {
			row = append(row,
				seg{fmt.Sprintf("%8s ", fmtSecs(s.Offset, true)), offsetStyle(s.Offset)},
				seg{fmt.Sprintf("%7s", fmtSecs(s.Err, false)), dim})
		}
		lines = append(lines, row)
	}
	return lines
}

func trackingLines(st *Stats, _ int) []line {
	t := st.Tracking
	if t == nil {
		return unavailable("Tracking", st.TrackingErr)
	}
	leapSt := red
	if t.Leap == "Normal" {
		leapSt = green
	}
	ago := time.Since(time.Unix(0, int64(t.RefTime*1e9))).Seconds()
	return []line{
		kv("Reference", fmt.Sprintf("%s (%s)", t.Name, t.RefID), cyan),
		kv("Stratum", t.Stratum, plain),
		kv("Leap status", t.Leap, leapSt),
		kv("Last update", fmtDur(math.Max(0, ago))+" ago", plain),
		kv("System time", fmtSecs(math.Abs(t.SysTime), false)+" "+fastSlow(t.SysTime, false), offsetStyle(t.SysTime)),
		kv("Last offset", fmtSecs(t.LastOffset, true), offsetStyle(t.LastOffset)),
		kv("RMS offset", fmtSecs(t.RMSOffset, false), offsetStyle(t.RMSOffset)),
		kv("Frequency", fmt.Sprintf("%.3f ppm %s", math.Abs(t.Freq), fastSlow(t.Freq, true)), plain),
		kv("Skew", fmt.Sprintf("%.3f ppm", t.Skew), plain),
		kv("Root delay", fmtSecs(t.RootDelay, false), plain),
		kv("Root dispersion", fmtSecs(t.RootDispersion, false), plain),
	}
}

func unavailable(label string, err error) []line {
	lines := []line{{{label + " unavailable", red}}}
	if err != nil {
		lines = append(lines, line{{clean(err.Error()), red}})
	}
	return lines
}

func netLines(st *Stats, _ int) []line {
	if len(st.Net) < 4 {
		return []line{{{st.Iface + ": no counters", red}}}
	}
	r := st.NetRate
	if len(r) < 4 {
		r = []float64{0, 0, 0, 0}
	}
	return []line{
		kv("Interface", st.Iface, cyan),
		kv("RX", fmt.Sprintf("%9s  %6s pkt/s", fmtSI(r[0]*8, "bps"), fmtSI(r[1], "")), green),
		kv("TX", fmt.Sprintf("%9s  %6s pkt/s", fmtSI(r[2]*8, "bps"), fmtSI(r[3], "")), cyan),
		kv("RX since boot", fmtBytes(st.Net[0]), plain),
		kv("TX since boot", fmtBytes(st.Net[2]), plain),
	}
}

func systemLines(st *Stats, _ int) []line {
	memPct := 0.0
	if st.MemTotal > 0 {
		memPct = st.MemUsed / st.MemTotal * 100
	}
	chronyd := "not found"
	if st.ChronydRSS > 0 {
		chronyd = fmt.Sprintf("%.1f%% CPU  %s", st.ChronydCPU, fmtBytes(st.ChronydRSS))
	}
	cpu := append(kv("CPU", "", plain), meter(st.CPU)...)
	mem := append(kv("Memory", "", plain), meter(memPct)...)
	return []line{
		append(cpu, seg{fmt.Sprintf("%.1f%%", st.CPU), plain}),
		append(mem, seg{fmtBytes(st.MemUsed) + " / " + fmtBytes(st.MemTotal), plain}),
		kv("Load", loadAvg(), plain),
		kv("chronyd", chronyd, cyan),
		kv("chronytop", fmt.Sprintf("%.1f%% CPU", st.OwnCPU), dim),
	}
}

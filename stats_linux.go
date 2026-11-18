package main

import (
	"os"
	"strconv"
	"strings"
)

// USER_HZ is 100 on all mainstream Linux architectures.
const clockTicks = 100

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

// cpuSampler computes system CPU use from the change in /proc/stat between calls.
type cpuSampler struct{ busy, total float64 }

func (c *cpuSampler) percent() (float64, bool) {
	busy, total := cpuTicks()
	ok := c.total > 0 && total > c.total
	pct := 0.0
	if ok {
		pct = (busy - c.busy) / (total - c.total) * 100
	}
	c.busy, c.total = busy, total
	return pct, ok
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

package main

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// macOS has no /proc, so system stats come from its standard tools, run by absolute path.
func run(name string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// #nosec G204 -- fixed system binaries; args are constants, an interface name or a pid.
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "LC_ALL=C"}
	out, _ := cmd.Output()
	return string(out)
}

func sysctl(name string) string {
	return strings.TrimSpace(run("/usr/sbin/sysctl", "-n", name))
}

func defaultIface() string {
	for _, line := range strings.Split(run("/sbin/route", "-n", "get", "default"), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), ":"); ok && k == "interface" {
			return strings.TrimSpace(v)
		}
	}
	return "en0"
}

// netCounters returns rx bytes, rx packets, tx bytes and tx packets.
func netCounters(iface string) []float64 {
	if iface == "" || strings.HasPrefix(iface, "-") {
		return nil
	}
	// Columns: Name Mtu Network [Address] Ipkts Ierrs Ibytes Opkts Oerrs Obytes Coll.
	// Address is blank for some interfaces, so count from the end.
	for _, line := range strings.Split(run("/usr/sbin/netstat", "-ibn", "-I", iface), "\n") {
		f := strings.Fields(line)
		if n := len(f); n >= 10 && f[0] == iface && strings.HasPrefix(f[2], "<Link#") {
			return []float64{num(f[n-5]), num(f[n-7]), num(f[n-2]), num(f[n-4])}
		}
	}
	return nil
}

// cpuSampler reports the summed %CPU of all processes, as ps estimates it, over all cores.
type cpuSampler struct{ ncpu float64 }

func (c *cpuSampler) percent() (float64, bool) {
	if c.ncpu == 0 {
		c.ncpu = max(1, num(sysctl("hw.ncpu")))
	}
	var sum float64
	for _, v := range strings.Fields(run("/bin/ps", "-A", "-o", "%cpu=")) {
		sum += num(v)
	}
	return sum / c.ncpu, true
}

func memInfo() (total, used float64) {
	total = num(sysctl("hw.memsize"))
	out := run("/usr/bin/vm_stat")
	pageSize := 4096.0
	var avail float64
	for i, line := range strings.Split(out, "\n") {
		if i == 0 {
			// "Mach Virtual Memory Statistics: (page size of 16384 bytes)"
			if _, rest, ok := strings.Cut(line, "page size of "); ok {
				if f := strings.Fields(rest); len(f) > 0 {
					pageSize = num(f[0])
				}
			}
			continue
		}
		k, v, _ := strings.Cut(line, ":")
		switch k {
		case "Pages free", "Pages inactive", "Pages speculative", "Pages purgeable":
			avail += num(strings.TrimSuffix(strings.TrimSpace(v), "."))
		}
	}
	if total == 0 {
		return 0, 0
	}
	return total, max(0, total-avail*pageSize)
}

func findPID(name string) string {
	f := strings.Fields(run("/usr/bin/pgrep", "-x", name))
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

// procUsage returns CPU seconds and resident memory for a pid.
func procUsage(pid string) (secs, rss float64, ok bool) {
	if _, err := strconv.Atoi(pid); err != nil {
		return 0, 0, false
	}
	f := strings.Fields(run("/bin/ps", "-o", "time=,rss=", "-p", pid))
	if len(f) < 2 {
		return 0, 0, false
	}
	return parseCPUTime(f[0]), num(f[1]) * 1024, true
}

// parseCPUTime parses ps's [dd-][hh:]mm:ss.ss format.
func parseCPUTime(s string) float64 {
	var days float64
	if d, rest, ok := strings.Cut(s, "-"); ok {
		days, s = num(d), rest
	}
	var secs float64
	for _, part := range strings.Split(s, ":") {
		secs = secs*60 + num(part)
	}
	return days*86400 + secs
}

func loadAvg() string {
	f := strings.Fields(strings.Trim(sysctl("vm.loadavg"), "{} "))
	if len(f) < 3 {
		return ""
	}
	return strings.Join(f[:3], " ")
}

func uptime() float64 {
	// "{ sec = 1794013357, usec = 803881 } Fri Nov  6 17:02:37 2026"
	_, rest, ok := strings.Cut(sysctl("kern.boottime"), "sec = ")
	if !ok {
		return 0
	}
	sec, _, _ := strings.Cut(rest, ",")
	boot := num(sec)
	if boot == 0 {
		return 0
	}
	return float64(time.Now().Unix()) - boot
}

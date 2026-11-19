// Command chronytop is a btop-style terminal dashboard for a chrony NTP server.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

var version = "dev"

const (
	clientsInterval = 10 * time.Second
	minInterval     = 500 * time.Millisecond
	maxInterval     = 10 * time.Second
)

type panelDef struct{ key, title string }

var panels = []panelDef{
	{"1", "requests"}, {"2", "server"}, {"3", "offenders"}, {"4", "sources"},
	{"5", "tracking"}, {"6", "net"}, {"7", "system"},
}

// Panel keys per column, by column count. Offenders fills the rest of its column.
var layouts = map[int][][]string{
	1: {{"2", "5", "3", "4", "6", "7"}},
	2: {{"2", "4", "6", "7"}, {"5", "3"}},
	3: {{"2", "6", "7"}, {"4", "5"}, {"3"}},
}

// App is the dashboard state.
type App struct {
	stats      Stats
	scr        Screen
	visible    map[string]bool
	interval   time.Duration
	minPackets int
	help       bool
	host       string
	network    string
	address    string
	fallback   bool
}

func main() {
	interval := flag.Duration("i", time.Second, "sample interval (500ms to 10s)")
	minPackets := flag.Int("p", 100, "minimum packets for a client to be listed in offenders")
	socketPath := flag.String("socket", defaultSocket, "absolute path to chronyd's Unix command socket")
	udpHost := flag.String("host", "", "monitor chronyd over UDP: hostname, IP, or host:port (default port 323)")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("chronytop", version)
		return
	}
	if *minPackets < 0 || uint64(*minPackets) > math.MaxUint32 {
		fmt.Fprintln(os.Stderr, "chronytop: -p must fit an unsigned 32-bit integer")
		os.Exit(2)
	}
	intervalFlag, socketFlag := false, false
	flag.Visit(func(f *flag.Flag) {
		intervalFlag = intervalFlag || f.Name == "i"
		socketFlag = socketFlag || f.Name == "socket"
	})
	network, address, err := monitorAddress(*socketPath, *udpHost, socketFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, "chronytop:", err)
		os.Exit(2)
	}
	fallback := false
	if network == "unixgram" && !socketFlag {
		// Like chronyc, use localhost UDP when the default socket isn't accessible.
		if c, err := openChrony(address); err == nil {
			c.Close()
		} else if errors.Is(err, os.ErrPermission) {
			network, address, fallback = "udp", "127.0.0.1:323", true
		}
	}

	host, _ := os.Hostname()
	iface := ""
	if isRemote(network, address) {
		host = address
	} else {
		iface = defaultIface()
	}
	app := &App{
		stats:      Stats{Iface: iface},
		network:    network,
		address:    address,
		fallback:   fallback,
		visible:    map[string]bool{},
		interval:   min(maxInterval, max(minInterval, *interval)),
		minPackets: *minPackets,
		host:       clean(host),
	}
	for _, p := range panels {
		app.visible[p.key] = true
	}
	if saved, ok := loadSettings(); ok {
		if saved.Shown != nil {
			for _, p := range panels {
				app.visible[p.key] = saved.Shown[p.title]
			}
		}
		if saved.Interval > 0 && !intervalFlag {
			app.interval = saved.Interval
		}
	}
	before := app.settings().String()

	fd := os.Stdin.Fd()
	old, err := makeRaw(fd)
	if err != nil {
		fmt.Fprintln(os.Stderr, "chronytop: must be run in a terminal")
		os.Exit(1)
	}
	_, _ = io.WriteString(os.Stdout, "\x1b[?1049h\x1b[?25l")
	defer func() {
		_, _ = io.WriteString(os.Stdout, "\x1b[0m\x1b[?25h\x1b[?1049l")
		restore(fd, old)
	}()
	app.run()
	// Only write the settings file when something changed.
	if s := app.settings(); s.String() != before {
		_ = saveSettings(s)
	}
}

func (a *App) settings() Settings {
	s := Settings{Shown: map[string]bool{}, Interval: a.interval}
	for _, p := range panels {
		s.Shown[p.title] = a.visible[p.key]
	}
	return s
}

type clientsResult struct {
	clients []Client
	err     error
}

func (a *App) run() {
	ctx, cancel := context.WithCancel(context.Background())
	sampler := &chronySession{network: a.network, address: a.address, fallback: a.fallback}
	clientSession := &chronySession{network: a.network, address: a.address, fallback: a.fallback}
	var workers sync.WaitGroup
	defer func() {
		cancel()
		workers.Wait()
		sampler.Close()
		clientSession.Close()
	}()
	keys := make(chan byte, 16)
	go readKeys(keys)
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGWINCH, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)
	results := make(chan clientsResult, 1)
	fetching, nextClients := false, time.Time{}
	minPackets := a.minPackets
	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		select {
		case <-timer.C:
			a.stats.Sample(ctx, sampler)
			// The client list can be long, so fetch it in the background and less often.
			if !fetching && a.visible["3"] && time.Now().After(nextClients) {
				fetching = true
				workers.Add(1)
				go func() {
					defer workers.Done()
					c, err := clientSession.clients(ctx, minPackets)
					results <- clientsResult{c, err}
				}()
			}
			timer.Reset(a.interval)
		case r := <-results:
			fetching, nextClients = false, time.Now().Add(clientsInterval)
			a.stats.Clients, a.stats.ClientsErr, a.stats.ClientsAt = r.clients, r.err, time.Now()
		case k, ok := <-keys:
			if !ok || a.handleKey(k) {
				return
			}
		case sig := <-sigs:
			if sig != syscall.SIGWINCH {
				return
			}
		}
		w, h := termSize(os.Stdout.Fd())
		a.draw(w, h)
		_ = a.scr.Flush(os.Stdout)
	}
}

// readKeys sends key presses; escape sequences (arrow keys etc.) are ignored.
func readKeys(keys chan<- byte) {
	buf := make([]byte, 32)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil {
			close(keys)
			return
		}
		if n > 1 && buf[0] == 27 {
			continue
		}
		for _, k := range buf[:n] {
			keys <- k
		}
	}
}

// handleKey returns true to quit.
func (a *App) handleKey(k byte) bool {
	switch k {
	case 'q', 'Q', 3: // 3 is Ctrl-C.
		return true
	case 27: // Esc
		if !a.help {
			return true
		}
		a.help = false
	case '?', 'h', 'H':
		a.help = !a.help
	case '+', '=':
		a.interval = min(maxInterval, a.interval*2)
	case '-':
		a.interval = max(minInterval, a.interval/2)
	default:
		if _, ok := a.visible[string(k)]; ok && a.panelAvailable(string(k)) {
			a.visible[string(k)] = !a.visible[string(k)]
		}
	}
	return false
}

func (a *App) panelAvailable(key string) bool {
	return !isRemote(a.network, a.address) || (key != "6" && key != "7")
}

package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// replySocketMu covers the process-wide umask change around bind.
var replySocketMu sync.Mutex

// Wire layout references (protocol version 6):
// https://github.com/mlichvar/chrony/blob/4.9/candm.h
// https://github.com/mlichvar/chrony/blob/4.9/pktlength.c
// Server statistics also use the reply layouts from chrony 4.0 and 4.2.
// Distributors can match chrony's compiled-in socket path without patching the
// source: go build -ldflags "-X main.defaultSocket=/run/chrony/chronyd.sock" .
// The -socket flag takes precedence over this build-time default.
var defaultSocket = "/var/run/chrony/chronyd.sock"

const (
	cmdSources  = 14
	cmdSource   = 15
	cmdTracking = 33
	cmdServer   = 54
	cmdClients  = 68
	replyHeader = 28
	maxClients  = 8
	clientSize  = 60
	// syscall.O_PATH is absent on linux/386, linux/amd64 and linux/arm
	// (verified with Go 1.24 and 1.27). Its Linux ABI value is 0x200000 on
	// all our targets: 386, amd64, arm, arm64 and riscv64.
	linuxOPath = 0x200000
)

var wire = binary.BigEndian

// Request sizes (padded to the largest reply, as protocol version 6 requires),
// and data lengths for each accepted reply type.
var commands = map[uint16]struct {
	size    int
	replies map[uint16]int
}{
	cmdSources:  {32, map[uint16]int{2: 4}},
	cmdSource:   {76, map[uint16]int{3: 48}},
	cmdTracking: {104, map[uint16]int{5: 76}},
	cmdServer:   {196, map[uint16]int{22: 32, 24: 44, 25: 168}}, // chrony 4.0, 4.2, 4.3+
	cmdClients:  {520, map[uint16]int{21: 492}},
}

// chronySession owns one persistent socket. It is used serially by one worker;
// the sampler and the client-list worker each have their own session.
type chronySession struct {
	network  string
	address  string
	fallback bool // UDP because the default socket was inaccessible
	socket   *chronySocket
}

// run renews the deadline for a complete query group, including all pages.
// Failed sessions are discarded; the next scheduled refresh reconnects, without
// a retry loop that could put extra load on an unavailable daemon.
func (s *chronySession) run(ctx context.Context, timeout time.Duration, query func(*chronySocket) error) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	if d, exists := ctx.Deadline(); exists && d.Before(deadline) {
		deadline = d
	}
	if s.socket == nil {
		if s.network == "udp" {
			s.socket, err = openChronyUDP(ctx, s.address, deadline)
			if err == nil {
				s.socket.fallback = s.fallback
			}
		} else {
			s.socket, err = openChrony(s.address)
		}
		if err != nil {
			return err
		}
	}
	c := s.socket
	c.deadline = deadline
	if err := c.conn.SetDeadline(deadline); err != nil {
		s.Close()
		return err
	}
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		_ = c.conn.Close()
		close(done)
	})
	defer func() {
		// A running cancellation callback must finish before this socket can be
		// reused or replaced; otherwise it could close a later query's socket.
		if !stop() {
			<-done
		}
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		if err != nil && !commandErrorsOnly(err) {
			s.Close()
		}
	}()
	return query(c)
}

// Close is called by the owner, after its worker has stopped.
func (s *chronySession) Close() {
	if s.socket != nil {
		s.socket.Close()
		s.socket = nil
	}
}

type chronySocket struct {
	conn      net.Conn
	buf       []byte // reply buffer, reused for every query
	udp       bool
	fallback  bool
	deadline  time.Time
	parentFD  int
	replyName string // basename of the reply socket, beside chronyd's socket
	replyPath string
	replyDev  uint64
	replyIno  uint64
	sequence  uint32
}

func openChronyUDP(ctx context.Context, address string, deadline time.Time) (*chronySocket, error) {
	dialer := net.Dialer{Deadline: deadline}
	conn, err := dialer.DialContext(ctx, "udp", address)
	if err != nil {
		return nil, fmt.Errorf("connect to chronyd at %s: %w", address, err)
	}
	// A connected UDP socket filters the sender address. Randomising the initial
	// sequence also makes off-path reply guessing harder (it is not authentication).
	var sequence [4]byte
	if _, err := rand.Read(sequence[:]); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return &chronySocket{conn: conn, udp: true, parentFD: -1, sequence: wire.Uint32(sequence[:])}, nil
}

func openChrony(path string) (*chronySocket, error) {
	c, err := prepareChronySocket(path)
	if err != nil {
		return nil, err
	}
	if err := c.connectUnix(path); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

func prepareChronySocket(path string) (*chronySocket, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("chrony socket path must be absolute")
	}
	// Keep the reply socket beside chronyd's socket: /tmp may be in a different
	// mount namespace when chronyd runs with systemd PrivateTmp.
	// O_PATH requires search permission, not permission to list the parent.
	parent, err := syscall.Open(filepath.Dir(path), linuxOPath|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open chronyd socket directory (check permissions or use sudo): %w", err)
	}
	c := &chronySocket{parentFD: parent}
	ok := false
	defer func() {
		if !ok {
			c.Close()
		}
	}()
	// One path component, not a private subdirectory. Debian and Ubuntu ship an
	// AppArmor profile that allows chronyd to reply to @{run}/chrony/{,*} and
	// denies sendmsg into a nested directory, so the reply never arrives.
	// Pin the parent so cleanup cannot be redirected onto a replaced directory.
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, err
	}
	c.replyName = fmt.Sprintf(".chronytop-%x", random)
	c.replyPath = filepath.Join(filepath.Dir(path), c.replyName)
	// sockaddr_un.sun_path holds 108 bytes including the terminating NUL.
	if len(c.replyPath) >= 108 {
		return nil, errors.New("chronyd socket path is too long for a reply socket")
	}
	ok = true
	return c, nil
}

func (c *chronySocket) connectUnix(path string) error {
	// Use the real pathname for bind: chronyd resolves the return address in its
	// own process, so a /proc/self/fd path would point at the daemon's descriptors.
	// Socket permissions come from umask at bind. fchmod on the descriptor
	// changes the sockfs inode, not the directory entry chronyd sends to.
	conn, err := dialReplySocket(c.replyPath, path)
	if err != nil {
		// Bind succeeds before connect. A failed connect leaves the reply
		// socket behind; a failed bind must not delete a name we did not create.
		c.removeFreshReply()
		return fmt.Errorf("connect to chronyd at %s: %w", path, err)
	}
	c.conn = conn
	var st syscall.Stat_t
	if err := syscall.Lstat(fmt.Sprintf("/proc/self/fd/%d/%s", c.parentFD, c.replyName), &st); err != nil {
		_ = syscall.Unlinkat(c.parentFD, c.replyName)
		return err
	}
	if st.Mode&0o777 != 0o666 {
		_ = syscall.Unlinkat(c.parentFD, c.replyName)
		return fmt.Errorf("unexpected reply socket mode %o", st.Mode&0o777)
	}
	c.replyDev, c.replyIno = uint64(st.Dev), uint64(st.Ino)
	return nil
}

func dialReplySocket(local, remote string) (*net.UnixConn, error) {
	replySocketMu.Lock()
	defer replySocketMu.Unlock()
	// 0777 &^ 0111 = 0666, so chronyd can send the reply. Restored immediately.
	old := syscall.Umask(0o111)
	defer syscall.Umask(old)
	return net.DialUnix("unixgram", &net.UnixAddr{Name: local, Net: "unixgram"},
		&net.UnixAddr{Name: remote, Net: "unixgram"})
}

func (c *chronySocket) Close() {
	c.removeReplySocket()
	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
	}
	if c.parentFD >= 0 {
		_ = syscall.Close(c.parentFD)
		c.parentFD = -1
	}
}

// removeFreshReply unlinks a reply socket left behind by a failed dial.
// It ignores names this process did not bind, including a symlink or file
// planted at the reply path.
func (c *chronySocket) removeFreshReply() {
	st, ok := c.statReply()
	if !ok || st.Mode&syscall.S_IFMT != syscall.S_IFSOCK || st.Mode&0o777 != 0o666 || int(st.Uid) != syscall.Geteuid() {
		return
	}
	_ = syscall.Unlinkat(c.parentFD, c.replyName)
}

// removeReplySocket unlinks the reply socket only when the directory entry is
// still the inode we bound. The parent is owned by chronyd, which can replace
// the name; that replacement is left alone.
func (c *chronySocket) removeReplySocket() {
	if c.parentFD < 0 || c.replyName == "" || c.replyIno == 0 {
		return
	}
	current, ok := c.statReply()
	if !ok || uint64(current.Dev) != c.replyDev || uint64(current.Ino) != c.replyIno {
		return
	}
	_ = syscall.Unlinkat(c.parentFD, c.replyName)
}

func (c *chronySocket) statReply() (syscall.Stat_t, bool) {
	var st syscall.Stat_t
	if c.parentFD < 0 || c.replyName == "" {
		return st, false
	}
	// Resolve through the pinned directory fd. Lstat does not follow a symlink
	// that has replaced the reply socket.
	err := syscall.Lstat(fmt.Sprintf("/proc/self/fd/%d/%s", c.parentFD, c.replyName), &st)
	return st, err == nil
}

// query sends only the read-only commands used by this dashboard.
func (c *chronySocket) query(command uint16, args ...uint32) ([]byte, uint16, error) {
	cmd, ok := commands[command]
	if !ok {
		return nil, 0, errors.New("unsupported chrony command")
	}
	request := make([]byte, cmd.size)
	request[0], request[1] = 6, 1
	wire.PutUint16(request[4:], command)
	c.sequence++
	wire.PutUint32(request[8:], c.sequence)
	for i, arg := range args {
		if 20+4*i+4 > len(request) {
			return nil, 0, errors.New("too many chrony command arguments")
		}
		wire.PutUint32(request[20+4*i:], arg)
	}
	attempt := uint16(0)
	retryDelay := 250 * time.Millisecond
	send := func() error {
		if c.udp {
			// Retry lost UDP packets without extending the query group's deadline.
			until := time.Now().Add(retryDelay)
			if c.deadline.Before(until) {
				until = c.deadline
			}
			if err := c.conn.SetReadDeadline(until); err != nil {
				return err
			}
		}
		wire.PutUint16(request[6:], attempt)
		_, err := c.conn.Write(request)
		return err
	}
	if err := send(); err != nil {
		return nil, 0, err
	}
	if c.buf == nil {
		c.buf = make([]byte, 4096)
	}
	buf := c.buf
	for {
		n, err := c.conn.Read(buf)
		if err != nil {
			var netErr net.Error
			if c.udp && errors.As(err, &netErr) && netErr.Timeout() && attempt < 2 && time.Now().Before(c.deadline) {
				attempt++
				retryDelay *= 2
				if err := send(); err != nil {
					return nil, 0, err
				}
				continue
			}
			return nil, 0, err
		}
		if n < replyHeader {
			return nil, 0, errors.New("truncated chrony reply header")
		}
		if wire.Uint32(buf[16:]) != c.sequence || wire.Uint16(buf[4:]) != command {
			continue // An unrelated or delayed reply must not become a sample.
		}
		if buf[0] != 6 || buf[1] != 2 || buf[2] != 0 || buf[3] != 0 {
			return nil, 0, errors.New("unsupported chrony reply header or protocol version")
		}
		if status := wire.Uint16(buf[8:]); status != 0 {
			return nil, 0, &chronyCommandError{command: command, status: status, udp: c.udp, fallback: c.fallback}
		}
		kind := wire.Uint16(buf[6:])
		length, ok := cmd.replies[kind]
		if !ok {
			return nil, 0, fmt.Errorf("unsupported chrony reply %d for %s", kind, chronyCommandName(command))
		}
		if n < replyHeader+length {
			return nil, 0, errors.New("truncated chrony reply data")
		}
		return buf[replyHeader:n], kind, nil
	}
}

type chronyCommandError struct {
	command, status uint16
	udp, fallback   bool
}

func (e *chronyCommandError) Error() string {
	message := chronyStatus(e.status)
	if e.status == 2 && e.fallback {
		message = "not authorised over UDP (use sudo, or -socket if it's elsewhere)"
	} else if e.status == 2 && e.udp {
		message = "not authorised over UDP (check chronyd cmdallow/opencommands)"
	}
	return fmt.Sprintf("chronyd %s: %s", chronyCommandName(e.command), message)
}

func chronyCommandName(command uint16) string {
	switch command {
	case cmdSources:
		return "sources"
	case cmdSource:
		return "source data"
	case cmdTracking:
		return "tracking"
	case cmdServer:
		return "serverstats"
	case cmdClients:
		return "clients"
	default:
		return fmt.Sprintf("command %d", command)
	}
}

// A valid rejection (e.g. serverstats is not enabled over UDP) does not mean
// the socket is broken. Handle errors.Join from a sample with several commands.
func commandErrorsOnly(err error) bool {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, child := range joined.Unwrap() {
			if !commandErrorsOnly(child) {
				return false
			}
		}
		return true
	}
	_, ok := err.(*chronyCommandError)
	return ok
}

func chronyStatus(status uint16) string {
	switch status {
	case 2:
		return "not authorised (check socket permissions or use sudo)"
	case 3:
		return "command not supported"
	case 4:
		return "source no longer exists"
	case 6:
		return "client logging is disabled"
	case 18:
		return "protocol version not supported"
	case 19:
		return "invalid request length"
	default:
		return fmt.Sprintf("status %d", status)
	}
}

// Chrony's Float is not IEEE 754: it has a signed 7-bit exponent and
// signed 25-bit coefficient, with no implicit leading bit.
func chronyFloat(b []byte) float64 {
	x := wire.Uint32(b)
	exponent := int(x >> 25)
	if exponent >= 64 {
		exponent -= 128
	}
	coefficient := int32(x & 0x1ffffff)
	if coefficient >= 0x1000000 {
		coefficient -= 0x2000000
	}
	return math.Ldexp(float64(coefficient), exponent-25)
}

func chronyIP(b []byte) string {
	switch wire.Uint16(b[16:]) {
	case 1:
		return net.IP(b[:4]).String()
	case 2:
		return net.IP(b[:16]).String()
	default:
		return ""
	}
}

func refName(b []byte) string { return clean(string(b[:4])) }

func (c *chronySocket) Tracking() (*Tracking, error) {
	b, _, err := c.query(cmdTracking)
	if err != nil {
		return nil, err
	}
	name := chronyIP(b[4:24])
	if wire.Uint16(b[20:]) == 0 {
		name = refName(b)
	}
	hi := wire.Uint32(b[28:])
	if hi == 0x7fffffff { // Legacy 32-bit timestamp marker.
		hi = 0
	}
	seconds := uint64(hi)<<32 | uint64(wire.Uint32(b[32:]))
	leaps := []string{"Normal", "Insert second", "Delete second", "Not synchronised"}
	leap := "Unknown"
	if i := int(wire.Uint16(b[26:])); i < len(leaps) {
		leap = leaps[i]
	}
	return &Tracking{
		RefID: fmt.Sprintf("%08X", wire.Uint32(b)), Name: name,
		Stratum: strconv.Itoa(int(wire.Uint16(b[24:]))), Leap: leap,
		RefTime: float64(seconds) + float64(min(wire.Uint32(b[36:]), 999999999))/1e9,
		SysTime: chronyFloat(b[40:]), LastOffset: chronyFloat(b[44:]), RMSOffset: chronyFloat(b[48:]),
		Freq: chronyFloat(b[52:]), Skew: chronyFloat(b[60:]),
		RootDelay: chronyFloat(b[64:]), RootDispersion: chronyFloat(b[68:]),
	}, nil
}

func (c *chronySocket) Sources() ([]Source, error) {
	b, _, err := c.query(cmdSources)
	if err != nil {
		return nil, err
	}
	var sources []Source
	for i, count := uint32(0), wire.Uint32(b); i < count; i++ {
		b, _, err := c.query(cmdSource, i)
		if err != nil {
			return nil, err
		}
		if wire.Uint16(b[16:]) == 3 { // Unresolved sources are hidden by chronyc too.
			continue
		}
		mode, state := " ", " "
		if m := int(wire.Uint16(b[26:])); m < 3 {
			mode = string("^=#"[m])
		}
		if s := int(wire.Uint16(b[24:])); s < 6 {
			state = string("*?x~+-"[s])
		}
		name := chronyIP(b)
		if mode == "#" && wire.Uint16(b[16:]) == 1 {
			name = refName(b)
		}
		poll := int(wire.Uint16(b[20:]))
		if poll >= 1<<15 {
			poll -= 1 << 16
		}
		sources = append(sources, Source{
			Mode: mode, State: state, Name: name, Poll: poll,
			Stratum: strconv.Itoa(int(wire.Uint16(b[22:]))), Reach: strconv.FormatUint(uint64(wire.Uint16(b[30:])), 8),
			LastRx: float64(wire.Uint32(b[32:])), Offset: chronyFloat(b[40:]), Err: chronyFloat(b[44:]),
		})
	}
	return sources, nil
}

func (c *chronySocket) ServerStats() ([]float64, error) {
	b, kind, err := c.query(cmdServer)
	if err != nil {
		return nil, err
	}
	count := 8
	if kind == 24 {
		count = 11
	} else if kind == 25 {
		count = 17
	}
	// Preserve the panel's counter order (the same order as chronyc's report).
	order := []int{0, 3, 2, 5, 6, 1, 4, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	stats := make([]float64, count)
	for i := range stats {
		if kind == 25 {
			stats[i] = float64(wire.Uint64(b[8*order[i]:]))
		} else {
			stats[i] = float64(wire.Uint32(b[4*order[i]:]))
		}
	}
	return stats, nil
}

func (s *chronySession) clients(ctx context.Context, minPackets int) ([]Client, error) {
	var clients []Client
	err := s.run(ctx, 10*time.Second, func(c *chronySocket) error {
		var err error
		clients, err = c.Clients(minPackets)
		return err
	})
	if err != nil {
		return nil, err
	}
	return clients, nil
}

func (c *chronySocket) Clients(minPackets int) ([]Client, error) {
	if minPackets < 0 || uint64(minPackets) > math.MaxUint32 {
		return nil, errors.New("minimum packets must fit an unsigned 32-bit integer")
	}
	var clients []Client
	for index := uint32(0); ; {
		// reset=0 is essential: monitoring must never reset chronyd's counters.
		b, _, err := c.query(cmdClients, index, maxClients, uint32(minPackets), 0)
		if err != nil {
			return nil, err
		}
		total, next, count := wire.Uint32(b), wire.Uint32(b[4:]), wire.Uint32(b[8:])
		if count > maxClients {
			return nil, errors.New("invalid chrony client count")
		}
		for i := uint32(0); i < count; i++ {
			record := b[12+int(i)*clientSize:]
			name := chronyIP(record)
			if name == "" {
				continue
			}
			interval := int(record[44])
			if interval >= 128 {
				interval -= 256
			}
			clients = append(clients, Client{Name: name, Packets: float64(wire.Uint32(record[20:])),
				Dropped: float64(wire.Uint32(record[32:])), Interval: interval})
		}
		if next >= total || count < maxClients {
			return clients, nil
		}
		if next <= index {
			return nil, errors.New("chrony client pagination did not advance")
		}
		index = next
	}
}

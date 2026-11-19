package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// This opt-in test exercises our client against a real daemon. Expected values
// come from the isolated configuration and generated traffic, not CLI output.
func TestChronydIntegration(t *testing.T) {
	binary := os.Getenv("CHRONYTOP_TEST_CHRONYD")
	if binary == "" {
		t.Skip("set CHRONYTOP_TEST_CHRONYD to test an isolated chronyd")
	}
	version, err := exec.Command(binary, "-v").CombinedOutput()
	if err != nil {
		t.Fatalf("chronyd version: %v: %s", err, version)
	}
	t.Logf("%s", bytes.TrimSpace(version))
	match := regexp.MustCompile(`version (\d+)\.(\d+)`).FindSubmatch(version)
	if len(match) != 3 {
		t.Fatalf("unrecognised chronyd version: %s", version)
	}
	major, _ := strconv.Atoi(string(match[1]))
	minor, _ := strconv.Atoi(string(match[2]))
	for _, open := range []bool{false, true} {
		t.Run(fmt.Sprintf("opencommands=%t", open), func(t *testing.T) {
			if open && (major < 4 || major == 4 && minor < 7) {
				t.Skip("opencommands requires chrony 4.7 or later")
			}
			socket, commandAddr, ntpAddr := startTestChronyd(t, binary, open)
			sendTestNTP(t, ntpAddr)
			for _, network := range []string{"unixgram", "udp"} {
				t.Run(network, func(t *testing.T) {
					address := socket
					if network == "udp" {
						address = commandAddr
					}
					s := &chronySession{network: network, address: address}
					defer s.Close()
					var first *chronySocket
					// A second pass must reuse the connection and leave NTP counters intact.
					for i := 0; i < 2; i++ {
						err := s.run(context.Background(), 2*time.Second, func(c *chronySocket) error {
							if first != nil && first != c {
								t.Error("connection was not reused")
							}
							first = c
							tracking, err := c.Tracking()
							if err != nil {
								return err
							}
							if tracking.Stratum != "8" || tracking.Leap != "Normal" {
								t.Errorf("local reference: %+v", tracking)
							}
							sources, err := c.Sources()
							if err != nil {
								return err
							}
							if len(sources) != 1 || sources[0].Name != "192.0.2.1" || sources[0].Mode != "^" || sources[0].Reach != "0" || sources[0].Poll != 6 {
								t.Errorf("offline source: %+v", sources)
							}
							server, serverErr := c.ServerStats()
							clients, clientsErr := c.Clients(0)
							if network == "udp" && !open {
								for _, err := range []error{serverErr, clientsErr} {
									var denied *chronyCommandError
									if !errors.As(err, &denied) || denied.status != 2 {
										t.Errorf("restricted UDP command: got %v, want not authorised", err)
									}
								}
								return errors.Join(serverErr, clientsErr)
							}
							if err := errors.Join(serverErr, clientsErr); err != nil {
								return err
							}
							if len(server) < 8 || server[0] != 1 || server[1] != 0 {
								t.Errorf("NTP counters: %v", server)
							}
							if len(clients) != 1 || clients[0].Name != "127.0.0.1" || clients[0].Packets != 1 || clients[0].Dropped != 0 {
								t.Errorf("NTP client: %+v", clients)
							}
							// chronyd applies this threshold to both NTP and command
							// packets, including our UDP monitoring requests.
							filtered, err := c.Clients(100)
							if len(filtered) != 0 {
								t.Errorf("minimum packet filter: %+v", filtered)
							}
							return err
						})
						if err != nil && !(network == "udp" && !open && commandErrorsOnly(err)) {
							t.Fatal(err)
						}
					}
				})
			}
		})
	}
}

func startTestChronyd(t *testing.T, binary string, open bool) (string, string, string) {
	t.Helper()
	// Short paths are necessary for sockaddr_un's 108-byte limit.
	dir, err := os.MkdirTemp("", "ct-live-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	var ports [2]*net.UDPConn
	for i := range ports {
		ports[i], err = net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ports[i].Close() })
	}
	commandAddr, ntpAddr := ports[0].LocalAddr().String(), ports[1].LocalAddr().String()
	socket := filepath.Join(dir, "chronyd.sock")
	// The local reference supplies deterministic tracking data. The documentation
	// address stays offline and only supplies a Sources record; it is never polled.
	config := fmt.Sprintf(`pidfile %s/pid
bindcmdaddress %s
bindcmdaddress 127.0.0.1
cmdport %d
bindaddress 127.0.0.1
port %d
allow 127.0.0.1
local stratum 8
server 192.0.2.1 offline minpoll 6 maxpoll 6
`, dir, socket, ports[0].LocalAddr().(*net.UDPAddr).Port, ports[1].LocalAddr().(*net.UDPAddr).Port)
	if open {
		config += "opencommands activity manual rtcdata smoothing sourcename sources sourcestats tracking serverstats clients\n"
	}
	path := filepath.Join(dir, "chrony.conf")
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	// -x prevents clock changes; no system configuration or online sources are
	// loaded. -U permits this unprivileged setup; explicit -u keeps all test-owned
	// files accessible to this user instead of dropping to a distribution user.
	cmd := exec.Command(binary, "-d", "-x", "-U", "-u", current.Username, "-f", path)
	var log bytes.Buffer
	cmd.Stdout, cmd.Stderr = &log, &log
	for _, port := range ports {
		_ = port.Close()
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
		if t.Failed() {
			t.Logf("chronyd log:\n%s", log.String())
		}
	})
	s := &chronySession{address: socket}
	defer s.Close()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-done:
			t.Fatalf("chronyd exited during startup:\n%s", log.String())
		default:
		}
		err = s.run(context.Background(), 100*time.Millisecond, func(c *chronySocket) error {
			tracking, err := c.Tracking()
			if err == nil && tracking.Stratum != "8" {
				return errors.New("local reference not active yet")
			}
			return err
		})
		if err == nil {
			return socket, commandAddr, ntpAddr
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("chronyd did not become ready: %v", err)
	return "", "", ""
}

func sendTestNTP(t *testing.T, address string) {
	t.Helper()
	c, err := net.Dial("udp4", address)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	request := make([]byte, 48)
	request[0] = 4<<3 | 3 // NTPv4 client request.
	request[47] = 1
	if _, err := c.Write(request); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, 512)
	n, err := c.Read(reply)
	if err != nil {
		t.Fatal(err)
	}
	if n < 48 || reply[0]&7 != 4 || !bytes.Equal(reply[24:32], request[40:48]) {
		t.Fatalf("invalid NTP response: %x", reply[:n])
	}
}

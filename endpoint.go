package main

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"path/filepath"
	"strconv"
	"strings"
)

// monitorAddress returns the transport and address selected by -socket or -host.
func monitorAddress(socketPath, host string, socketExplicit bool) (string, string, error) {
	if host == "" {
		if !filepath.IsAbs(socketPath) {
			return "", "", errors.New("-socket must be an absolute path")
		}
		return "unixgram", socketPath, nil
	}
	if socketExplicit {
		return "", "", errors.New("-host and -socket cannot be used together")
	}
	name, port, err := net.SplitHostPort(host)
	if err != nil {
		name, port, err = net.SplitHostPort(host + ":323")
	}
	if _, ipErr := netip.ParseAddr(host); err != nil && ipErr == nil { // bare IPv6 address
		name, port, err = host, "323", nil
	}
	n, portErr := strconv.Atoi(port)
	if err != nil || portErr != nil || n < 1 || n > 65535 || name == "" || strings.ContainsAny(name, "/\\ \t\r\n") {
		return "", "", fmt.Errorf("invalid -host %q (use host, host:port or [IPv6]:port)", host)
	}
	return "udp", net.JoinHostPort(name, strconv.Itoa(n)), nil
}

// isRemote reports whether chronyd may run on another machine, so local panels don't apply.
func isRemote(network, address string) bool {
	if network != "udp" {
		return false
	}
	host, _, _ := net.SplitHostPort(address)
	ip, err := netip.ParseAddr(host)
	return host != "localhost" && (err != nil || !ip.Unmap().IsLoopback())
}

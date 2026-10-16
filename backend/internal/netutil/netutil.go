// Package netutil provides small networking helpers shared across relume-tv's
// discovery components (SSDP, mDNS announcer, diagnostics).
package netutil

import (
	"fmt"
	"log"
	"log/slog"
	"net"
	"strings"
)

// InterfaceForIP returns the multicast-capable, up interface that carries the
// given IP. It errors if ip is not a valid IP or if no such interface is found.
func InterfaceForIP(ip string) (*net.Interface, error) {
	target := net.ParseIP(ip)
	if target == nil {
		return nil, fmt.Errorf("invalid IP %q", ip)
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	for i := range ifaces {
		if ifaces[i].Flags&net.FlagMulticast == 0 || ifaces[i].Flags&net.FlagUp == 0 {
			continue
		}
		addrs, aerr := ifaces[i].Addrs()
		if aerr != nil {
			continue
		}
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.Equal(target) {
				return &ifaces[i], nil
			}
		}
	}
	return nil, fmt.Errorf("no multicast-capable interface with IP %s", ip)
}

// virtualPrefixes are interface names that never carry the host's own LAN identity:
// container bridges and veth pairs (present under network_mode: host), libvirt
// bridges, tunnels.
var virtualPrefixes = []string{"lo", "docker", "veth", "br-", "virbr", "tun", "tap", "wg", "tailscale", "utun"}

// PrimaryMAC returns the hardware address of the lowest-index interface that is
// up, not loopback, not a known virtual interface, and has a 6-byte MAC. ok is
// false when no such interface exists.
func PrimaryMAC() (net.HardwareAddr, bool) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, false
	}
	return primaryMAC(ifaces)
}

func primaryMAC(ifaces []net.Interface) (net.HardwareAddr, bool) {
	var best *net.Interface
	for i := range ifaces {
		f := &ifaces[i]
		if f.Flags&net.FlagUp == 0 || f.Flags&net.FlagLoopback != 0 || len(f.HardwareAddr) != 6 {
			continue
		}
		if isVirtualName(f.Name) {
			continue
		}
		if best == nil || f.Index < best.Index {
			best = f
		}
	}
	if best == nil {
		return nil, false
	}
	return best.HardwareAddr, true
}

func isVirtualName(name string) bool {
	for _, p := range virtualPrefixes {
		if len(name) >= len(p) && name[:len(p)] == p {
			return true
		}
	}
	return false
}

// MDNSLogger adapts hashicorp/mdns's *log.Logger sink (server and client) onto
// our slog.Logger. Without it the library writes to Go's default logger, i.e.
// raw stderr lines outside the structured log.
func MDNSLogger(sl *slog.Logger) *log.Logger {
	return log.New(mdnsLogFilter{log: sl}, "", 0)
}

// mdnsLogFilter: bind failures at WARN (the responder is deaf then), everything
// else at DEBUG. The announce server binds the shared mDNS multicast group, so
// it sees — and tries to handle — every mDNS packet on the LAN: each query with
// the TC bit (a known-answer list, RFC 6762 §7.2, any iOS/macOS browser) and
// each NSEC record miekg/dns cannot unpack ("buffer size too small") is an
// "[ERR]" line with the packet dumped into it. The server skips that one packet
// and keeps answering; at WARN that stream buries the log.
//
// Dropped outright: "Closing client", the query client's struct dump on every
// Close (the setup wizard polls discovery).
type mdnsLogFilter struct{ log *slog.Logger }

func (w mdnsLogFilter) Write(p []byte) (int, error) {
	line := strings.TrimRight(string(p), "\n")
	switch {
	case strings.Contains(line, "Closing client"):
	case strings.Contains(line, "Failed to bind"):
		w.log.Warn(line)
	default:
		w.log.Debug(line)
	}
	return len(p), nil
}

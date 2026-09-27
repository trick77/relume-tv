// Package mdns actively announces relume-tv as a Hue bridge via mDNS/Bonjour
// (_hue._tcp.local.). Modern Philips TVs (and the Hue Bridge Pro itself) find the
// bridge primarily this way; they passively listen for the announcement and
// often make no request of their own. The format follows hass-emulated-hue,
// which the Ambilight TV is known to discover: instance name
// "Philips Hue - XXXXXX", TXT with bridgeid and modelid=BSB002.
package mdns

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/hashicorp/mdns"
	"github.com/trick77/relume-tv/internal/config"
	"github.com/trick77/relume-tv/internal/netutil"
)

const (
	service = "_hue._tcp"
	domain  = "local."
)

// Announcer keeps the mDNS registration alive.
type Announcer struct {
	id    config.Identity
	advIP string
	port  int
	log   *slog.Logger
	// BurstDuration enables a diagnostic re-announcement burst after startup.
	// Defaults to disabled.
	BurstDuration time.Duration
	// BurstInterval is the interval used during the diagnostic burst.
	BurstInterval time.Duration
}

type serviceSpec struct {
	instance string
	service  string
	domain   string
	host     string
	txt      []string
}

// New creates an Announcer. port is the advertised SRV port (usually the
// HTTP port of the emulated bridge).
func New(id config.Identity, advIP string, port int, log *slog.Logger) *Announcer {
	return &Announcer{id: id, advIP: advIP, port: port, log: log}
}

// Run registers the service and keeps it announced until ctx is cancelled.
func (a *Announcer) Run(ctx context.Context) error {
	spec := a.serviceSpec()

	var iface *net.Interface
	if i, err := netutil.InterfaceForIP(a.advIP); err != nil {
		a.log.Warn("mdns: interface for advertise IP not found, using all", "err", err)
	} else {
		iface = i
	}

	// A real Gen-2 Hue bridge is IPv4-only. Passing an explicit IPv4-only IPs list
	// to NewMDNSService announces only an A record (no AAAA) — relying on the
	// host's interface addresses would also publish IPv6 AAAA records, which a
	// real bridge never has and which some TVs reject or mis-handle.
	ip := net.ParseIP(a.advIP)
	if ip == nil {
		return fmt.Errorf("mdns: invalid advertise IP %q", a.advIP)
	}
	svc, err := mdns.NewMDNSService(spec.instance, spec.service, spec.domain, spec.host+"."+spec.domain, a.port, []net.IP{ip}, spec.txt)
	if err != nil {
		return fmt.Errorf("mdns service: %w", err)
	}

	server, err := mdns.NewServer(&mdns.Config{
		Zone:   svc,
		Iface:  iface,
		Logger: log.New(mdnsLogFilter{log: a.log}, "", 0),
	})
	if err != nil {
		return fmt.Errorf("mdns register: %w", err)
	}
	// hashicorp/mdns's Shutdown() just closes the sockets — it does not
	// multicast an mDNS "goodbye" (records with TTL 0) which would otherwise
	// evice relume-tv from the TV's cache.
	_ = server
	a.log.Info("mdns: announced as hue bridge",
		"instance", spec.instance, "host", spec.host+"."+spec.domain, "ip", a.advIP, "port", a.port, "bridgeid", a.id.BridgeID())

	// Register exactly once and keep the responder alive; hashicorp/mdns answers
	// the TV's active _hue._tcp queries from here on.
	//
	// We deliberately do NOT periodically re-register. The Ambilight TV actively
	// queries _hue._tcp (confirmed by packet capture) and caches the answer, so
	// re-registering risks the same TV-side flicker seen previously with
	// grandcat/zeroconf's goodbye-on-Shutdown behavior. The confirmed-working
	// ha-hue-entertainment emulator also registers exactly once. This is why
	// relume-tv served an identical descriptor yet was never listed.
	if a.BurstDuration > 0 {
		// The startup discovery burst is handled by the SSDP responder. mDNS needs
		// no burst: the TV queries actively, and a real re-announce here would only
		// emit harmful goodbye packets (see above).
		a.log.Info("mdns: registered once; discovery burst handled via SSDP (no mDNS goodbye)")
	}
	<-ctx.Done()
	return ctx.Err()
}

func (a *Announcer) serviceSpec() serviceSpec {
	bridgeID := a.id.BridgeID()
	host := a.id.Serial
	return serviceSpec{
		instance: "Philips Hue - " + bridgeID[len(bridgeID)-6:],
		service:  service,
		domain:   domain,
		// Unique, bridge-like hostname for the SRV target / A record so it never
		// collides with the host's own mDNS name (e.g. nas.local).
		host: host,
		txt: []string{
			"bridgeid=" + bridgeID,
			"modelid=BSB002",
		},
	}
}

// mdnsLogFilter adapts hashicorp/mdns's *log.Logger sink onto our slog.Logger,
// and drops "buffer size too small" unpack failures (typically
// "NSEC.NextDomain: dns: buffer size too small"). Our announce server binds
// the shared mDNS multicast group, so it sees — and tries to unpack — every
// mDNS packet on the LAN, not just Hue-related ones; a known miekg/dns gap in
// parsing certain NSEC records (e.g. another device's RFC 6762 §6.1 negative
// response) then fires repeatedly for as long as that device keeps
// broadcasting. It's cosmetic: the server just skips that one malformed
// packet and keeps answering every other query fine, so it doesn't warrant
// WARN-level noise on every occurrence. Everything else still passes through.
type mdnsLogFilter struct{ log *slog.Logger }

func (w mdnsLogFilter) Write(p []byte) (int, error) {
	line := strings.TrimRight(string(p), "\n")
	if strings.Contains(line, "buffer size too small") {
		return len(p), nil
	}
	w.log.Warn("mdns", "msg", line)
	return len(p), nil
}

// Package mdns answers for relume-tv as a Hue bridge via mDNS/Bonjour
// (_hue._tcp.local.). Modern Philips TVs find the bridge primarily this way:
// the TV actively queries _hue._tcp (confirmed by packet capture) and caches
// the answer; hashicorp/mdns replies to each querier and never announces
// unsolicited. The format follows hass-emulated-hue, which the Ambilight TV is
// known to discover: instance name "Philips Hue - XXXXXX", TXT with bridgeid
// and modelid=BSB002.
package mdns

import (
	"context"
	"fmt"
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

	ip := net.ParseIP(a.advIP)
	if ip == nil {
		return fmt.Errorf("mdns: invalid advertise IP %q", a.advIP)
	}
	svc, err := newService(spec, ip, a.port)
	if err != nil {
		return fmt.Errorf("mdns service: %w", err)
	}

	server, err := mdns.NewServer(&mdns.Config{
		Zone:   svc,
		Iface:  iface,
		Logger: netutil.MDNSLogger(a.log),
	})
	if err != nil {
		return fmt.Errorf("mdns register: %w", err)
	}
	// hashicorp/mdns's Shutdown() just closes the sockets — it does not
	// multicast an mDNS "goodbye" (records with TTL 0) which would otherwise
	// evict relume-tv from the TV's cache.
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

// newService builds the zone hashicorp/mdns answers from.
//
// A real Gen-2 Hue bridge is IPv4-only. Passing an explicit IPv4-only IPs list
// announces only an A record (no AAAA) — relying on the host's interface
// addresses would also publish IPv6 AAAA records, which a real bridge never has
// and which some TVs reject or mis-handle.
//
// The instance name goes in escaped ("Philips\ Hue\ -\ XXXXXX"): miekg/dns hands
// a queried name over in that form, and the zone compares it byte for byte
// against the instance name it was built with. Unescaped, a direct SRV/TXT query
// for the instance (the TV's cache refresh at 80–95% of the 120s TTL) gets no
// answer and the bridge drops off the Ambilight list. On the wire both forms
// are the same label bytes.
func newService(spec serviceSpec, ip net.IP, port int) (*mdns.MDNSService, error) {
	return mdns.NewMDNSService(escapeLabel(spec.instance), spec.service, spec.domain, spec.host+"."+spec.domain, port, []net.IP{ip}, spec.txt)
}

// escapeLabel escapes a single DNS label the way miekg/dns presents it: the
// characters it treats as special in a name (space, dot, backslash, and the
// rest of its isDomainNameLabelSpecial set that can appear in a Hue instance).
func escapeLabel(label string) string {
	var b strings.Builder
	for i := 0; i < len(label); i++ {
		switch c := label[i]; c {
		case ' ', '.', '\\', '"', '\'', ';', '@', '(', ')':
			b.WriteByte('\\')
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
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

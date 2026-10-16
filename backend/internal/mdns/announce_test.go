package mdns

import (
	"io"
	"log/slog"
	"net"
	"reflect"
	"testing"

	"github.com/miekg/dns"
	"github.com/trick77/relume-tv/internal/config"
)

func TestServiceSpec_matchesHueBridgeAnnouncement(t *testing.T) {
	// Given
	a := New(config.Identity{Serial: "2c4d54ea2832"}, "192.0.2.10", 80, slog.New(slog.NewTextHandler(io.Discard, nil)))

	// When
	spec := a.serviceSpec()

	// Then
	if spec.instance != "Philips Hue - EA2832" {
		t.Errorf("instance = %q", spec.instance)
	}
	if spec.service != "_hue._tcp" {
		t.Errorf("service = %q", spec.service)
	}
	if spec.domain != "local." {
		t.Errorf("domain = %q", spec.domain)
	}
	if spec.host != "2c4d54ea2832" {
		t.Errorf("host = %q", spec.host)
	}
	if !reflect.DeepEqual(spec.txt, []string{"bridgeid=2C4D54FFFEEA2832", "modelid=BSB002"}) {
		t.Errorf("txt = %#v", spec.txt)
	}
}

// The TV refreshes its cache with direct SRV/TXT queries for the instance name;
// the name arrives from the wire the way miekg/dns unpacks it (spaces escaped),
// and hashicorp/mdns's zone compares it byte for byte.
func TestNewService_answersDirectInstanceQueriesWithSpacesInTheName(t *testing.T) {
	// Given
	a := New(config.Identity{Serial: "2c4d54ea2832"}, "192.0.2.10", 80, slog.New(slog.NewTextHandler(io.Discard, nil)))
	svc, err := newService(a.serviceSpec(), net.ParseIP("192.0.2.10"), 80)
	if err != nil {
		t.Fatalf("newService: %v", err)
	}
	// A query for the instance as a client puts it on the wire: raw label bytes.
	q := new(dns.Msg)
	q.SetQuestion(`Philips\ Hue\ -\ EA2832._hue._tcp.local.`, dns.TypeSRV)
	wire, err := q.Pack()
	if err != nil {
		t.Fatalf("pack: %v", err)
	}
	var got dns.Msg
	if err := got.Unpack(wire); err != nil {
		t.Fatalf("unpack: %v", err)
	}

	// When
	recs := svc.Records(got.Question[0])

	// Then
	if len(recs) == 0 {
		t.Fatalf("no answer for %q", got.Question[0].Name)
	}
	srv, ok := recs[0].(*dns.SRV)
	if !ok || srv.Port != 80 || srv.Target != "2c4d54ea2832.local." {
		t.Errorf("first record = %v, want SRV port 80 target 2c4d54ea2832.local.", recs[0])
	}
	// The PTR answer for a browse carries the same raw label bytes on the wire.
	ptr := svc.Records(dns.Question{Name: "_hue._tcp.local.", Qtype: dns.TypePTR, Qclass: dns.ClassINET})
	if len(ptr) == 0 {
		t.Fatal("no PTR answer for the service browse")
	}
	m := new(dns.Msg)
	m.Answer = ptr
	if _, err := m.Pack(); err != nil {
		t.Fatalf("pack browse answer: %v", err)
	}
	if ptr[0].(*dns.PTR).Ptr != `Philips\ Hue\ -\ EA2832._hue._tcp.local.` {
		t.Errorf("PTR target = %q", ptr[0].(*dns.PTR).Ptr)
	}
}

func TestEscapeLabel(t *testing.T) {
	if got := escapeLabel("Philips Hue - EA2832"); got != `Philips\ Hue\ -\ EA2832` {
		t.Errorf("escapeLabel = %q", got)
	}
	if got := escapeLabel("plain"); got != "plain" {
		t.Errorf("escapeLabel(plain) = %q", got)
	}
}

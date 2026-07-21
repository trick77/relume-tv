package ssdp

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/trick77/relume-tv/internal/config"
)

// captureResponder returns a Responder whose log output is captured as JSON
// records, so tests can assert on what the debug logging actually reports.
func captureResponder(buf *bytes.Buffer) *Responder {
	r := New(
		config.Identity{Serial: "2c4d54ea2832"},
		"192.0.2.10", 80,
		slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
	)
	r.Debug = true
	return r
}

// logRecords decodes the captured JSON log lines.
func logRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not JSON: %q (%v)", line, err)
		}
		out = append(out, rec)
	}
	return out
}

func TestParseHeaders_uppercasesKeysAndTrimsValues(t *testing.T) {
	// Given: a realistic M-SEARCH with mixed-case keys and padded values
	msg := "M-SEARCH * HTTP/1.1\r\n" +
		"HOST: 239.255.255.250:1900\r\n" +
		"man: \"ssdp:discover\"\r\n" +
		"Mx:   3   \r\n" +
		"ST: upnp:rootdevice\r\n" +
		"User-Agent: Philips/TPM191E\r\n" +
		"\r\n"

	// When
	h := parseHeaders(msg)

	// Then: lookups elsewhere use uppercase keys, so parsing must normalise
	for key, want := range map[string]string{
		"HOST":       "239.255.255.250:1900",
		"MAN":        `"ssdp:discover"`,
		"MX":         "3",
		"ST":         "upnp:rootdevice",
		"USER-AGENT": "Philips/TPM191E",
	} {
		if got := h[key]; got != want {
			t.Errorf("h[%q] = %q, want %q", key, got, want)
		}
	}
	// The request line has no colon and must not become a header.
	if _, ok := h["M-SEARCH * HTTP/1.1"]; ok {
		t.Error("the request line leaked into the headers")
	}
}

func TestParseHeaders_ignoresMalformedLines(t *testing.T) {
	// Given: a blank line, a line without a colon, and a line starting with one
	// (an empty key) — none of which is a header
	msg := "NOTIFY * HTTP/1.1\r\n" +
		"\r\n" +
		"garbage-without-colon\r\n" +
		":leading-colon\r\n" +
		"NTS: ssdp:alive\r\n"

	// When
	h := parseHeaders(msg)

	// Then
	if len(h) != 1 || h["NTS"] != "ssdp:alive" {
		t.Fatalf("headers = %v, want only NTS", h)
	}
}

func TestParseHeaders_keepsColonsInTheValue(t *testing.T) {
	// Given: a LOCATION whose value itself contains colons — only the first
	// colon separates key from value
	msg := "LOCATION: http://192.0.2.10:80/description.xml\r\n"

	// When
	h := parseHeaders(msg)

	// Then
	if h["LOCATION"] != "http://192.0.2.10:80/description.xml" {
		t.Fatalf("LOCATION = %q", h["LOCATION"])
	}
}

func TestParseHeaders_emptyMessageYieldsNoHeaders(t *testing.T) {
	// When
	h := parseHeaders("")

	// Then: an empty map, never nil — callers index it directly
	if h == nil {
		t.Fatal("parseHeaders returned nil; callers index the result")
	}
	if len(h) != 0 {
		t.Fatalf("headers = %v, want empty", h)
	}
}

func TestLogDatagram_reportsFirstLineAndDiscoveryHeaders(t *testing.T) {
	// Given
	var buf bytes.Buffer
	r := captureResponder(&buf)
	src := &net.UDPAddr{IP: net.ParseIP("192.0.2.55"), Port: 41234}
	msg := "M-SEARCH * HTTP/1.1\r\n" +
		"MAN: \"ssdp:discover\"\r\n" +
		"ST: urn:schemas-upnp-org:device:basic:1\r\n" +
		"USER-AGENT: Philips TV\r\n" +
		"\r\n"

	// When
	r.logDatagram(src, msg)

	// Then: the whole point of this log is telling M-SEARCH from NOTIFY and
	// identifying the device, so those fields must be present.
	recs := logRecords(t, &buf)
	if len(recs) != 1 {
		t.Fatalf("got %d log records, want 1", len(recs))
	}
	rec := recs[0]
	if rec["line"] != "M-SEARCH * HTTP/1.1" {
		t.Errorf("line = %v, want the request line without the CRLF", rec["line"])
	}
	if rec["from"] != "192.0.2.55:41234" {
		t.Errorf("from = %v", rec["from"])
	}
	if rec["st"] != "urn:schemas-upnp-org:device:basic:1" {
		t.Errorf("st = %v", rec["st"])
	}
	if rec["man"] != `"ssdp:discover"` {
		t.Errorf("man = %v", rec["man"])
	}
	if rec["user-agent"] != "Philips TV" {
		t.Errorf("user-agent = %v", rec["user-agent"])
	}
	// Headers absent from an M-SEARCH log as empty, not as missing keys.
	if rec["nts"] != "" {
		t.Errorf("nts = %v, want empty for an M-SEARCH", rec["nts"])
	}
}

func TestLogDatagram_singleLineMessageHasNoCRLF(t *testing.T) {
	// Given: a datagram with no CRLF at all — the first-line split must not panic
	// or truncate
	var buf bytes.Buffer
	r := captureResponder(&buf)

	// When
	r.logDatagram(&net.UDPAddr{IP: net.ParseIP("192.0.2.56"), Port: 5000}, "NOTIFY * HTTP/1.1")

	// Then
	recs := logRecords(t, &buf)
	if len(recs) != 1 || recs[0]["line"] != "NOTIFY * HTTP/1.1" {
		t.Fatalf("line = %v", recs)
	}
}

// udpPair returns a listening responder-side conn and a client conn that has
// sent nothing yet. Loopback only — no multicast, so this cannot be flaky.
func udpPair(t *testing.T) (server *net.UDPConn, client *net.UDPConn) {
	t.Helper()
	server, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen server: %v", err)
	}
	client, err = net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		server.Close()
		t.Fatalf("listen client: %v", err)
	}
	t.Cleanup(func() { server.Close(); client.Close() })
	return server, client
}

// readAll drains datagrams from conn until a short read deadline expires.
func readAll(t *testing.T, conn *net.UDPConn) []string {
	t.Helper()
	var out []string
	buf := make([]byte, 4096)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			return out
		}
		out = append(out, string(buf[:n]))
	}
}

func TestHandle_answersMSearchWithAllVariants(t *testing.T) {
	// Given: a responder and a client address to answer to
	server, client := udpPair(t)
	var buf bytes.Buffer
	r := captureResponder(&buf)
	src := client.LocalAddr().(*net.UDPAddr)

	// When: an M-SEARCH arrives
	r.handle(server, src, []byte("M-SEARCH * HTTP/1.1\r\nMAN: \"ssdp:discover\"\r\nST: ssdp:all\r\n\r\n"))

	// Then: one 200 OK per advertised variant, sent to the querying address
	got := readAll(t, client)
	if len(got) != 3 {
		t.Fatalf("got %d responses, want one per SSDP variant (3): %q", len(got), got)
	}
	seenST := map[string]bool{}
	for _, resp := range got {
		if !strings.HasPrefix(resp, "HTTP/1.1 200 OK\r\n") {
			t.Errorf("response is not a 200 OK: %q", resp)
		}
		h := parseHeaders(resp)
		seenST[h["ST"]] = true
		// The TV follows LOCATION to fetch description.xml; a wrong host or port
		// there breaks discovery even though the M-SEARCH looked fine.
		if h["LOCATION"] != "http://192.0.2.10:80/description.xml" {
			t.Errorf("LOCATION = %q", h["LOCATION"])
		}
		if h["HUE-BRIDGEID"] != "2C4D54FFFEEA2832" {
			t.Errorf("hue-bridgeid = %q", h["HUE-BRIDGEID"])
		}
		if h["CACHE-CONTROL"] != "max-age=100" {
			t.Errorf("CACHE-CONTROL = %q", h["CACHE-CONTROL"])
		}
		if h["EXT"] != "" {
			t.Errorf("EXT must be present and empty, got %q", h["EXT"])
		}
	}
	for _, want := range []string{
		"upnp:rootdevice",
		"uuid:2f402f80-da50-11e1-9b23-2c4d54ea2832",
		"urn:schemas-upnp-org:device:basic:1",
	} {
		if !seenST[want] {
			t.Errorf("no response carried ST %q (got %v)", want, seenST)
		}
	}
}

func TestHandle_ignoresNonMSearchDatagrams(t *testing.T) {
	// Given: another device's own NOTIFY announcement on the multicast group
	server, client := udpPair(t)
	var buf bytes.Buffer
	r := captureResponder(&buf)
	src := client.LocalAddr().(*net.UDPAddr)

	// When
	r.handle(server, src, []byte("NOTIFY * HTTP/1.1\r\nNTS: ssdp:alive\r\nNT: upnp:rootdevice\r\n\r\n"))

	// Then: answering someone else's NOTIFY would spam the network
	if got := readAll(t, client); len(got) != 0 {
		t.Fatalf("responded to a NOTIFY with %d datagrams: %q", len(got), got)
	}
	// It is still logged in debug mode — that is what the observer is for.
	recs := logRecords(t, &buf)
	if len(recs) != 1 || recs[0]["nts"] != "ssdp:alive" {
		t.Fatalf("expected the NOTIFY to be logged, got %v", recs)
	}
}

func TestHandle_withDebugOffSendsResponsesButLogsNothing(t *testing.T) {
	// Given: the production default, Debug == false
	server, client := udpPair(t)
	var buf bytes.Buffer
	r := captureResponder(&buf)
	r.Debug = false
	src := client.LocalAddr().(*net.UDPAddr)

	// When
	r.handle(server, src, []byte("M-SEARCH * HTTP/1.1\r\nST: upnp:rootdevice\r\n\r\n"))

	// Then: discovery still works, it is just quiet
	if got := readAll(t, client); len(got) != 3 {
		t.Fatalf("got %d responses, want 3", len(got))
	}
	if buf.Len() != 0 {
		t.Errorf("expected no log output with Debug off, got %q", buf.String())
	}
}

func TestHandle_writeFailureStopsAfterTheFirstError(t *testing.T) {
	// Given: a closed connection, so every WriteToUDP fails
	server, client := udpPair(t)
	src := client.LocalAddr().(*net.UDPAddr)
	var buf bytes.Buffer
	r := captureResponder(&buf)
	server.Close()

	// When: handle must return rather than hammering a dead socket three times
	r.handle(server, src, []byte("M-SEARCH * HTTP/1.1\r\nST: ssdp:all\r\n\r\n"))

	// Then: exactly one "ssdp respond" warning, not one per variant
	var warnings int
	for _, rec := range logRecords(t, &buf) {
		if rec["msg"] == "ssdp respond" {
			warnings++
		}
	}
	if warnings != 1 {
		t.Fatalf("got %d respond warnings, want exactly 1 (it must bail on the first)", warnings)
	}
}

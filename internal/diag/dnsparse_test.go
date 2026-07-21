package diag

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"
)

// dnsHeader builds a 12-byte DNS header declaring qdCount questions.
func dnsHeader(qdCount int) []byte {
	return []byte{
		0x00, 0x00, // ID
		0x00, 0x00, // flags
		byte(qdCount >> 8), byte(qdCount), // QDCOUNT
		0x00, 0x00, // ANCOUNT
		0x00, 0x00, // NSCOUNT
		0x00, 0x00, // ARCOUNT
	}
}

// qname encodes a dotted name as length-prefixed labels terminated by a zero byte.
func qname(name string) []byte {
	var out []byte
	for _, label := range strings.Split(name, ".") {
		out = append(out, byte(len(label)))
		out = append(out, label...)
	}
	return append(out, 0x00)
}

// question encodes a full question section entry (name + QTYPE + QCLASS).
func question(name string) []byte {
	return append(qname(name), 0x00, 0x0c, 0x00, 0x01) // PTR, IN
}

func TestDNSQuestionNames_readsMultipleQuestions(t *testing.T) {
	// Given: a query for two services, as a TV enumerating the network sends
	msg := dnsHeader(2)
	msg = append(msg, question("_hue._tcp.local")...)
	msg = append(msg, question("_googlecast._tcp.local")...)

	// When
	names := dnsQuestionNames(msg)

	// Then
	if len(names) != 2 {
		t.Fatalf("names = %v, want 2", names)
	}
	if names[0] != "_hue._tcp.local" || names[1] != "_googlecast._tcp.local" {
		t.Fatalf("names = %v", names)
	}
}

func TestDNSQuestionNames_shortMessageYieldsNothing(t *testing.T) {
	// Given: a datagram too short to even hold a DNS header
	// When / Then
	for _, msg := range [][]byte{nil, {}, make([]byte, 11)} {
		if got := dnsQuestionNames(msg); got != nil {
			t.Errorf("dnsQuestionNames(%d bytes) = %v, want nil", len(msg), got)
		}
	}
}

func TestDNSQuestionNames_headerOnlyYieldsNothing(t *testing.T) {
	// Given: a valid header claiming zero questions (an mDNS response)
	// When
	names := dnsQuestionNames(dnsHeader(0))

	// Then
	if len(names) != 0 {
		t.Fatalf("names = %v, want none", names)
	}
}

func TestDNSQuestionNames_stopsWhenQDCountOverstatesTheBody(t *testing.T) {
	// Given: a header claiming five questions but carrying only one. A malicious
	// or truncated packet must not make the parser read past the buffer.
	msg := dnsHeader(5)
	msg = append(msg, question("_hue._tcp.local")...)

	// When
	names := dnsQuestionNames(msg)

	// Then: what was actually there, and nothing invented
	if len(names) != 1 || names[0] != "_hue._tcp.local" {
		t.Fatalf("names = %v, want just the one real question", names)
	}
}

func TestDNSQuestionNames_truncatedLabelIsRejected(t *testing.T) {
	// Given: a label claiming 20 bytes with only 3 present
	msg := dnsHeader(1)
	msg = append(msg, 20, 'h', 'u', 'e')

	// When
	names := dnsQuestionNames(msg)

	// Then
	if len(names) != 0 {
		t.Fatalf("names = %v, want none for a truncated label", names)
	}
}

func TestReadName_followsACompressionPointer(t *testing.T) {
	// Given: "_hue._tcp.local" at offset 12, then a name that is the single label
	// "b" followed by a pointer back to offset 12 — standard DNS compression.
	msg := dnsHeader(1)
	base := len(msg)
	msg = append(msg, qname("_hue._tcp.local")...)
	ptrAt := len(msg)
	msg = append(msg, 1, 'b', 0xC0, byte(base))

	// When
	name, next, ok := readName(msg, ptrAt)

	// Then
	if !ok {
		t.Fatal("readName failed on a valid compression pointer")
	}
	if name != "b._hue._tcp.local" {
		t.Fatalf("name = %q, want %q", name, "b._hue._tcp.local")
	}
	// next must point just past the 2-byte pointer, not past the target name.
	if next != ptrAt+4 {
		t.Fatalf("next = %d, want %d (past the pointer, not the target)", next, ptrAt+4)
	}
}

func TestReadName_pointerLoopTerminates(t *testing.T) {
	// Given: two pointers aimed at each other. On untrusted multicast input this
	// is the classic decompression-bomb; the jump budget must stop it.
	msg := dnsHeader(1)
	base := len(msg)
	msg = append(msg, 0xC0, byte(base+2), 0xC0, byte(base))

	// When: this must return rather than spin forever
	_, _, ok := readName(msg, base)

	// Then
	if ok {
		t.Fatal("a pointer loop must be rejected, not parsed")
	}
}

func TestReadName_selfPointerTerminates(t *testing.T) {
	// Given: a pointer to itself
	msg := dnsHeader(1)
	base := len(msg)
	msg = append(msg, 0xC0, byte(base))

	// When
	_, _, ok := readName(msg, base)

	// Then
	if ok {
		t.Fatal("a self-referential pointer must be rejected")
	}
}

func TestReadName_truncatedPointerIsRejected(t *testing.T) {
	// Given: a pointer's high byte at the very end of the buffer
	msg := append(dnsHeader(1), 0xC0)

	// When
	_, _, ok := readName(msg, 12)

	// Then
	if ok {
		t.Fatal("a pointer missing its second byte must be rejected")
	}
}

func TestReadName_unterminatedNameIsRejected(t *testing.T) {
	// Given: labels that run to the end of the buffer with no zero terminator
	msg := append(dnsHeader(1), qname("_hue._tcp.local")...)

	// When
	_, _, ok := readName(msg[:len(msg)-1], 12)

	// Then
	if ok {
		t.Fatal("a name without its terminating zero byte must be rejected")
	}
}

func TestReadName_rootNameIsEmpty(t *testing.T) {
	// Given: a bare zero byte — the DNS root
	msg := append(dnsHeader(1), 0x00)

	// When
	name, next, ok := readName(msg, 12)

	// Then
	if !ok || name != "" || next != 13 {
		t.Fatalf("readName(root) = %q, %d, %v", name, next, ok)
	}
}

// observerWithLog returns an observer logging JSON into buf.
func observerWithLog(buf *bytes.Buffer) *MDNSObserver {
	return NewMDNSObserver("192.0.2.10", slog.New(slog.NewJSONHandler(buf, nil)))
}

// loggedNames extracts the "name" field of every captured record.
func loggedNames(t *testing.T, buf *bytes.Buffer) []string {
	t.Helper()
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not JSON: %q", line)
		}
		if n, ok := rec["name"].(string); ok {
			out = append(out, n)
		}
	}
	return out
}

func TestInspect_logsHueQueriesFromAnyHost(t *testing.T) {
	// Given: an mDNS query for _hue._tcp from an arbitrary host
	var buf bytes.Buffer
	o := observerWithLog(&buf)
	msg := append(dnsHeader(1), question("_hue._tcp.local")...)
	src := &net.UDPAddr{IP: net.ParseIP("192.0.2.77"), Port: 5353}

	// When
	o.inspect(src, msg)

	// Then
	names := loggedNames(t, &buf)
	if len(names) != 1 || names[0] != "_hue._tcp.local" {
		t.Fatalf("logged names = %v", names)
	}
}

func TestInspect_ignoresUnrelatedQueriesByDefault(t *testing.T) {
	// Given: a Chromecast query, which is noise for this diagnostic
	var buf bytes.Buffer
	o := observerWithLog(&buf)
	msg := append(dnsHeader(1), question("_googlecast._tcp.local")...)

	// When
	o.inspect(&net.UDPAddr{IP: net.ParseIP("192.0.2.77"), Port: 5353}, msg)

	// Then
	if buf.Len() != 0 {
		t.Fatalf("expected silence for a non-Hue query, got %q", buf.String())
	}
}

func TestInspect_logsEveryQuestionFromTheConfiguredTVIP(t *testing.T) {
	// Given: DebugTVIP set — the point is seeing everything that TV asks for,
	// Hue-related or not, to find out how it actually discovers bridges
	var buf bytes.Buffer
	o := observerWithLog(&buf)
	o.DebugTVIP = "192.0.2.88"
	msg := dnsHeader(2)
	msg = append(msg, question("_googlecast._tcp.local")...)
	msg = append(msg, question("_airplay._tcp.local")...)

	// When
	o.inspect(&net.UDPAddr{IP: net.ParseIP("192.0.2.88"), Port: 5353}, msg)

	// Then
	names := loggedNames(t, &buf)
	if len(names) != 2 {
		t.Fatalf("logged names = %v, want both questions", names)
	}

	// And: the same packet from a different host stays filtered
	buf.Reset()
	o.inspect(&net.UDPAddr{IP: net.ParseIP("192.0.2.99"), Port: 5353}, msg)
	if buf.Len() != 0 {
		t.Fatalf("expected silence from a non-TV host, got %q", buf.String())
	}
}

func TestInspect_emptyQuestionListLogsNothing(t *testing.T) {
	// Given: DebugTVIP set but a packet carrying no questions at all
	var buf bytes.Buffer
	o := observerWithLog(&buf)
	o.DebugTVIP = "192.0.2.88"

	// When
	o.inspect(&net.UDPAddr{IP: net.ParseIP("192.0.2.88"), Port: 5353}, dnsHeader(0))

	// Then
	if buf.Len() != 0 {
		t.Fatalf("expected silence for a question-less packet, got %q", buf.String())
	}
}

func TestDeadline_isOneSecondAhead(t *testing.T) {
	// Given / When: the read deadline that keeps Run responsive to ctx cancellation
	d := deadline()

	// Then
	remaining := time.Until(d)
	if remaining <= 0 || remaining > 1100*time.Millisecond {
		t.Fatalf("deadline is %v ahead, want ~1s", remaining)
	}
}

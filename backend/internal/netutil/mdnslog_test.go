package netutil

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestMDNSLogger_dropsNoiseAndForwardsTheRest(t *testing.T) {
	// Given
	var buf bytes.Buffer
	l := MDNSLogger(slog.New(slog.NewTextHandler(&buf, nil)))

	// When
	l.Printf("[ERR] mdns: Failed to unpack packet: dns: overflow unpacking NSEC.NextDomain: dns: buffer size too small")
	l.Printf("[INFO] mdns: Closing client {true false ...}")
	l.Printf("[ERR] mdns: Failed to bind to udp port: address in use")

	// Then
	out := buf.String()
	if strings.Contains(out, "buffer size too small") || strings.Contains(out, "Closing client") {
		t.Errorf("noise was forwarded: %q", out)
	}
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "address in use") {
		t.Errorf("other line not forwarded at WARN: %q", out)
	}
	if strings.Contains(out, `\n"`) {
		t.Errorf("trailing newline not trimmed: %q", out)
	}
}

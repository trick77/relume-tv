package netutil

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestMDNSLogger_bindFailuresWarnTheRestDebugs(t *testing.T) {
	// Given
	var buf bytes.Buffer
	l := MDNSLogger(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	// When
	l.Printf("[INFO] mdns: Closing client {true false ...}")
	l.Printf("[ERR] mdns: Failed to handle query: [ERR] mdns: support for DNS requests with high truncated bit not implemented: {...}")
	l.Printf("[ERR] mdns: Failed to bind to udp4 port: address in use")

	// Then
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2 (Closing client dropped):\n%s", len(lines), buf.String())
	}
	if !strings.HasPrefix(strings.SplitN(lines[0], " ", 2)[1], "level=DEBUG") || !strings.Contains(lines[0], "truncated bit") {
		t.Errorf("handle-query line not at DEBUG: %q", lines[0])
	}
	if !strings.Contains(lines[1], "level=WARN") || !strings.Contains(lines[1], `msg="[ERR] mdns: Failed to bind to udp4 port: address in use"`) {
		t.Errorf("bind failure not at WARN as the message: %q", lines[1])
	}
	if strings.Contains(buf.String(), `\n"`) {
		t.Errorf("trailing newline not trimmed: %q", buf.String())
	}
}

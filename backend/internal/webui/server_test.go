package webui

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestServer_StateEndpoint(t *testing.T) {
	hub := NewHub(8)
	srv := NewServer(":0", hub, fakeSource{}, discardLog())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/state", nil)
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code = %d", rec.Code)
	}
	var snap Snapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if snap.Version != "1.4.2" {
		t.Fatalf("snapshot = %+v", snap)
	}
}

func TestServer_StateHasNoSecrets(t *testing.T) {
	hub := NewHub(8)
	srv := NewServer(":0", hub, fakeSource{}, discardLog())
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/state", nil))
	body := strings.ToLower(rec.Body.String())
	for _, banned := range []string{"appkey", "clientkey", "certsha", "psk", "username"} {
		if strings.Contains(body, banned) {
			t.Fatalf("state leaked %q: %s", banned, rec.Body.String())
		}
	}
}

// countingSource counts how often the snapshot builder reads it.
type countingSource struct {
	fakeSource
	reads *int32
}

func (c countingSource) Version() string {
	atomic.AddInt32(c.reads, 1)
	return c.fakeSource.Version()
}

func TestServer_SnapshotLoopIdleWithoutSubscribers(t *testing.T) {
	var reads int32
	srv := NewServer(":0", NewHub(8), countingSource{reads: &reads}, discardLog())
	srv.snapInterval = 5 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	go srv.runSnapshotLoop(ctx)
	time.Sleep(60 * time.Millisecond) // many ticks would have elapsed
	cancel()

	if n := atomic.LoadInt32(&reads); n != 0 {
		t.Fatalf("snapshot loop polled the source %d times with no subscribers — should be 0", n)
	}
}

func TestServer_PeriodicSnapshotPublished(t *testing.T) {
	hub := NewHub(8)
	srv := NewServer(":0", hub, fakeSource{}, discardLog())
	srv.snapInterval = 10 * time.Millisecond

	ch, cancel := hub.Subscribe()
	defer cancel()

	ctx, cancelCtx := context.WithCancel(context.Background())
	defer cancelCtx()
	go srv.runSnapshotLoop(ctx)

	deadline := time.After(2 * time.Second)
	for {
		select {
		case f := <-ch:
			if f.Kind == "snapshot" && f.Snapshot != nil && f.Snapshot.Version == "1.4.2" {
				return // a snapshot was published over time, not just on connect
			}
		case <-deadline:
			t.Fatal("no periodic snapshot frame received — dashboard would not update live")
		}
	}
}

func TestServer_SSEStreamsInitialSnapshot(t *testing.T) {
	hub := NewHub(8)
	srv := httptest.NewServer(NewServer(":0", hub, fakeSource{}, discardLog()).Handler())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "data: ") && strings.Contains(sc.Text(), "\"kind\":\"snapshot\"") {
			return // success
		}
	}
	t.Fatal("no snapshot frame received")
}

// A zero timeout means "no limit", which is the slow-loris exposure the
// explicit values exist to close. Asserting non-zero is what stops a later
// edit from quietly reverting them.
func TestNewHTTPServerSetsHeaderTimeoutOnly(t *testing.T) {
	s := &Server{addr: ":0"}
	srv := s.newHTTPServer()

	if srv.ReadHeaderTimeout == 0 {
		t.Error("ReadHeaderTimeout is 0, which means no limit")
	}
	if srv.IdleTimeout == 0 {
		t.Error("IdleTimeout is 0, which means no limit")
	}
	if srv.ReadTimeout != 0 {
		t.Errorf("ReadTimeout = %v, want 0: it would cut off large uploads and cancel long handlers", srv.ReadTimeout)
	}
	if srv.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %v, want 0: it would truncate the SSE stream", srv.WriteTimeout)
	}
	if srv.Handler == nil {
		t.Error("Handler is nil")
	}
}

func TestServer_HealthProbes(t *testing.T) {
	srv := NewServer(":0", NewHub(8), fakeSource{}, discardLog())
	for _, path := range []string{"/healthz", "/readyz"} {
		// The probes answer only this host: the UI port is LAN-reachable.
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = "127.0.0.1:40000"
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, rec.Code)
		}
		rec = httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s from the LAN = %d, want 404", path, rec.Code)
		}
	}
}

func TestServer_RunStopsWithOpenEventStream(t *testing.T) {
	// Given: a running UI server with a connected SSE client
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	srv := NewServer(addr, NewHub(8), fakeSource{}, discardLog())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	var resp *http.Response
	for range 50 {
		resp, err = http.Get("http://" + addr + "/api/events")
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// When
	start := time.Now()
	cancel()

	// Then: the open stream ends with the server instead of holding the drain
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Errorf("Run took %s to stop", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
}

package webui

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestServer_StateEndpoint(t *testing.T) {
	hub := NewHub(8)
	srv := NewServer(":0", hub, fakeSource{}, nil, discardLog())
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
	srv := NewServer(":0", hub, fakeSource{}, nil, discardLog())
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/state", nil))
	body := strings.ToLower(rec.Body.String())
	for _, banned := range []string{"appkey", "clientkey", "certsha", "psk", "username"} {
		if strings.Contains(body, banned) {
			t.Fatalf("state leaked %q: %s", banned, rec.Body.String())
		}
	}
}

func TestServer_FlashNilReturns404(t *testing.T) {
	srv := NewServer(":0", NewHub(8), fakeSource{}, nil, discardLog())
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/actions/flash", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404", rec.Code)
	}
}

func TestServer_FlashInvokesCallback(t *testing.T) {
	called := false
	srv := NewServer(":0", NewHub(8), fakeSource{}, func() error { called = true; return nil }, discardLog())
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/actions/flash", nil))
	if rec.Code != http.StatusNoContent || !called {
		t.Fatalf("code=%d called=%v", rec.Code, called)
	}
}

func TestServer_SSEStreamsInitialSnapshot(t *testing.T) {
	hub := NewHub(8)
	srv := httptest.NewServer(NewServer(":0", hub, fakeSource{}, nil, discardLog()).Handler())
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

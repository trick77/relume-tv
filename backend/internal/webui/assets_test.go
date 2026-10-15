package webui

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/trick77/relume-tv/web"
)

// requireBuiltUI fails rather than skips: a gate that ran without the UI copied
// into web/dist must not go green.
func requireBuiltUI(t *testing.T) {
	t.Helper()
	if _, err := fs.Stat(web.FS(), "index.html"); err != nil {
		t.Fatal("web/dist holds no UI: run `make ui` (or `make test`) from the repo root")
	}
}

func TestIndexServesAppShell(t *testing.T) {
	requireBuiltUI(t)
	srv := NewServer(":0", NewHub(1), fakeSource{}, discardLog())
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != 200 {
		t.Fatalf("code = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, marker := range []string{"app.js", "app.css", `id="app"`} {
		if !strings.Contains(body, marker) {
			t.Fatalf("index.html missing %q", marker)
		}
	}
	// Revalidated on every load, so an upgrade reaches an open browser.
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" || rec.Header().Get("ETag") == "" {
		t.Fatalf("shell Cache-Control = %q, ETag = %q", cc, rec.Header().Get("ETag"))
	}
	if rec.Header().Get("Content-Security-Policy") == "" {
		t.Fatal("no Content-Security-Policy on the shell")
	}
}

func TestBuiltUIRouting(t *testing.T) {
	srv := NewServer(":0", NewHub(1), fakeSource{}, discardLog())
	srv.assets = fstest.MapFS{
		"index.html":    {Data: []byte("<!doctype html>shell")},
		"app.js":        {Data: []byte("app()")},
		"assets/x-1.js": {Data: []byte("chunk()")},
	}
	h := srv.Handler()
	for _, tt := range []struct {
		path, body, cache string
		code              int
	}{
		{"/", "shell", "no-cache", 200},
		{"/setup", "shell", "no-cache", 200},
		// app.js is not content-hashed: no long-lived cache, an upgrade must replace it.
		{"/app.js", "app()", "", 200},
		{"/assets/x-1.js", "chunk()", "public, max-age=31536000, immutable", 200},
		{"/assets/gone.js", "", "", 404},
		{"/api/typo", "", "", 404},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tt.path, nil))
		if rec.Code != tt.code {
			t.Errorf("%s: code = %d, want %d", tt.path, rec.Code, tt.code)
			continue
		}
		if tt.code != 200 {
			continue
		}
		if !strings.Contains(rec.Body.String(), tt.body) {
			t.Errorf("%s: body = %q", tt.path, rec.Body.String())
		}
		if cc := rec.Header().Get("Cache-Control"); cc != tt.cache {
			t.Errorf("%s: Cache-Control = %q, want %q", tt.path, cc, tt.cache)
		}
	}
}

func TestAppJSReferencesEndpoints(t *testing.T) {
	requireBuiltUI(t)
	srv := NewServer(":0", NewHub(1), fakeSource{}, discardLog())
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/app.js", nil))
	body := rec.Body.String()
	for _, marker := range []string{"/api/state", "/api/events"} {
		if !strings.Contains(body, marker) {
			t.Fatalf("app.js missing %q", marker)
		}
	}
	// The test-flash action was removed; app.js must not reference its endpoint.
	if strings.Contains(body, "/api/actions/flash") {
		t.Fatalf("app.js still references the removed flash endpoint")
	}
}

func TestIndexWithoutBuiltUIExplains(t *testing.T) {
	srv := NewServer(":0", NewHub(1), fakeSource{}, discardLog())
	srv.assets = fstest.MapFS{".gitkeep": {}}
	h := srv.Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "make build") {
		t.Fatalf("code = %d, body = %q", rec.Code, rec.Body.String())
	}

	// The API stays up without the UI.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/state", nil))
	if rec.Code != 200 {
		t.Fatalf("/api/state code = %d", rec.Code)
	}
}

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
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "make build") {
		t.Fatalf("code = %d, body = %q", rec.Code, rec.Body.String())
	}

	// The API stays up without the UI.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/state", nil))
	if rec.Code != 200 {
		t.Fatalf("/api/state code = %d", rec.Code)
	}
}

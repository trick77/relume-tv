package bridgepro

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/trick77/relume-tv/internal/config"
)

// hostOf strips the "https://" scheme from an httptest server URL, leaving
// "127.0.0.1:PORT" as client.go expects (it builds "https://"+host+path).
func hostOf(t *testing.T, serverURL string) string {
	t.Helper()
	return strings.TrimPrefix(serverURL, "https://")
}

// pinOf computes the SHA-256 hex fingerprint of the server's leaf certificate,
// matching what VerifyPeerCertificate hashes (rawCerts[0] == cert.Raw).
func pinOf(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	cert := srv.Certificate()
	if cert == nil {
		t.Fatal("test server has no certificate")
	}
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:])
}

// pinnedClient builds a Client pinned to the given fingerprint against srv.
func pinnedClient(t *testing.T, srv *httptest.Server, certSHA256 string) *Client {
	t.Helper()
	return New(&config.BridgePro{
		Host:          hostOf(t, srv.URL),
		AppKey:        "test-app-key",
		CertSHA256:    certSHA256,
		SkipTLSVerify: false,
	})
}

func TestSetLight_DomainError(t *testing.T) {
	// 207 multi-status with HTTP 200 but a non-empty errors[] body.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"errors":[{"description":"color.xy not supported"}],"data":[]}`))
	}))
	defer srv.Close()

	c := pinnedClient(t, srv, pinOf(t, srv))
	err := c.SetLight("light-1", map[string]any{"color": map[string]any{"xy": map[string]any{"x": 0.5, "y": 0.5}}})
	if err == nil {
		t.Fatal("expected a domain error, got nil")
	}
	if !errors.Is(err, ErrDomain) {
		t.Fatalf("expected ErrDomain, got %v", err)
	}
	if !strings.Contains(err.Error(), "color.xy not supported") {
		t.Fatalf("expected description in message, got %v", err)
	}
}

func TestSetLight_DomainError_surfacesAllDescriptions(t *testing.T) {
	// A 207 multi-status can carry one error per attribute (e.g. several CT-only lights
	// each rejecting color.xy). Every description must reach the error, not just the first.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"errors":[{"description":"first failed"},{"description":"second failed"}],"data":[]}`))
	}))
	defer srv.Close()

	c := pinnedClient(t, srv, pinOf(t, srv))
	err := c.SetLight("light-1", map[string]any{"on": map[string]any{"on": true}})
	if err == nil || !errors.Is(err, ErrDomain) {
		t.Fatalf("expected ErrDomain, got %v", err)
	}
	if !strings.Contains(err.Error(), "first failed") || !strings.Contains(err.Error(), "second failed") {
		t.Fatalf("expected both descriptions in message, got %v", err)
	}
}

func TestSetLight_QueueFull(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`command queue is full`))
	}))
	defer srv.Close()

	c := pinnedClient(t, srv, pinOf(t, srv))
	err := c.SetLight("light-1", map[string]any{"on": map[string]any{"on": true}})
	if !errors.Is(err, ErrQueueFull) {
		t.Fatalf("expected ErrQueueFull, got %v", err)
	}
	if errors.Is(err, ErrUnreachable) || errors.Is(err, ErrDomain) {
		t.Fatalf("503 should be ErrQueueFull only, got %v", err)
	}
}

func TestSetLight_Unreachable(t *testing.T) {
	// Start a TLS server, grab its pin, then close it so the round-trip fails.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	pin := pinOf(t, srv)
	host := hostOf(t, srv.URL)
	srv.Close()

	c := New(&config.BridgePro{Host: host, AppKey: "k", CertSHA256: pin})
	err := c.SetLight("light-1", map[string]any{"on": map[string]any{"on": true}})
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("expected ErrUnreachable, got %v", err)
	}
}

func TestSetLight_OK(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"errors":[],"data":[{"rid":"light-1","rtype":"light"}]}`))
	}))
	defer srv.Close()

	c := pinnedClient(t, srv, pinOf(t, srv))
	if err := c.SetLight("light-1", map[string]any{"on": map[string]any{"on": true}}); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
}

func TestCertPinning_Match(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"errors":[]}`))
	}))
	defer srv.Close()

	c := pinnedClient(t, srv, pinOf(t, srv))
	if err := c.SetLight("light-1", map[string]any{"on": map[string]any{"on": true}}); err != nil {
		t.Fatalf("matching pin should succeed, got %v", err)
	}
}

func TestCertPinning_Mismatch(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"errors":[]}`))
	}))
	defer srv.Close()

	// Wrong (but non-empty) fingerprint with SkipTLSVerify=false so
	// VerifyPeerCertificate is installed and the pin check actually runs.
	wrong := strings.Repeat("00", sha256.Size)
	c := pinnedClient(t, srv, wrong)
	err := c.SetLight("light-1", map[string]any{"on": map[string]any{"on": true}})
	if err == nil {
		t.Fatal("wrong pin should fail")
	}
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("pin mismatch should be ErrUnreachable (Do fails), got %v", err)
	}
}

// TestCertPinning_ResumedSessionStillChecksPin is the regression test for a
// pinning bypass.
//
// VerifyPeerCertificate is NOT called on a resumed TLS session. With
// InsecureSkipVerify disabling the standard chain, a client that resumed a
// session therefore performed no certificate check at all, and the pin only
// ever applied to the very first handshake.
//
// The production client has no ClientSessionCache, so it does not resume today
// and the bypass is latent rather than live. That is exactly why this test
// drives the tls.Config the code builds rather than going through
// *http.Client: it pins the property (every handshake, fresh or resumed, is
// checked) instead of the current transport settings, so enabling a session
// cache later cannot silently reintroduce the hole.
func TestCertPinning_ResumedSessionStillChecksPin(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	// TLS 1.2: its ticket-based resumption is what skips VerifyPeerCertificate.
	srv.TLS = &tls.Config{MinVersion: tls.VersionTLS12, MaxVersion: tls.VersionTLS12}
	srv.StartTLS()
	defer srv.Close()

	host := hostOf(t, srv.URL)
	wrong := strings.Repeat("00", sha256.Size)

	// The config the production code builds, plus a session cache so a second
	// dial actually resumes.
	tr, ok := newHTTPClient(wrong, false).Transport.(*http.Transport)
	if !ok {
		t.Fatal("transport is not *http.Transport")
	}
	cfg := tr.TLSClientConfig.Clone()
	cfg.ClientSessionCache = tls.NewLRUClientSessionCache(4)
	cfg.ServerName = "127.0.0.1"

	dial := func() error {
		conn, err := tls.Dial("tcp", host+":443", cfg)
		if err != nil {
			return err
		}
		resumed := conn.ConnectionState().DidResume
		_ = conn.Close()
		if resumed {
			t.Log("handshake resumed a cached session")
		}
		return nil
	}

	// First handshake: rejected by the pin, and it seeds the session cache.
	if err := dial(); err == nil {
		t.Fatal("first handshake: a wrong pin must be rejected")
	}
	// Second handshake: must also be rejected. Before VerifyConnection existed,
	// a resumed session reached here with no certificate check at all.
	if err := dial(); err == nil {
		t.Fatal("second handshake: a wrong pin must be rejected on resumption too")
	}
}

// TestCertPinning_VerifyConnectionIsSet asserts the callback exists at all.
//
// It is the cheap half of the check above: VerifyConnection is the only hook
// that runs on a resumed handshake, so its absence is the bug, independent of
// whether a given test manages to trigger resumption.
func TestCertPinning_VerifyConnectionIsSet(t *testing.T) {
	tr, ok := newHTTPClient(strings.Repeat("00", sha256.Size), false).Transport.(*http.Transport)
	if !ok {
		t.Fatal("transport is not *http.Transport")
	}
	cfg := tr.TLSClientConfig
	if cfg.VerifyPeerCertificate == nil {
		t.Error("VerifyPeerCertificate is nil; fresh handshakes are unchecked")
	}
	if cfg.VerifyConnection == nil {
		t.Fatal("VerifyConnection is nil; a resumed session would skip the pin entirely")
	}
	// And it must actually reject a certificate that does not match the pin.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	if err := cfg.VerifyConnection(tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{srv.Certificate()},
	}); err == nil {
		t.Error("VerifyConnection accepted a certificate that does not match the pin")
	}

	// With no pin configured, neither callback is installed: that is the
	// documented skip-verify path, not an oversight.
	tr2, _ := newHTTPClient("", true).Transport.(*http.Transport)
	if tr2.TLSClientConfig.VerifyConnection != nil {
		t.Error("skip-verify must not install a pin check")
	}
}

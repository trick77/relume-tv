package bridgepro

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/trick77/relume-tv/internal/config"
)

func TestPair_returnsAppKeyAndClientKey(t *testing.T) {
	// Given: a Pro whose link button has been pressed
	var gotBody map[string]any
	var gotPath, gotContentType string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotContentType = r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		_, _ = w.Write([]byte(`[{"success":{"username":"app-key-123","clientkey":"DEADBEEF"}}]`))
	}))
	defer srv.Close()
	client := HTTPClientFor(&config.BridgePro{SkipTLSVerify: true})

	// When
	res, err := Pair(client, proHost(t, srv), "relume-tv#host")

	// Then
	if err != nil {
		t.Fatalf("Pair: %v", err)
	}
	if res.AppKey != "app-key-123" {
		t.Errorf("AppKey = %q", res.AppKey)
	}
	// The clientkey is the DTLS PSK; without it entertainment streaming is impossible.
	if res.ClientKey != "DEADBEEF" {
		t.Errorf("ClientKey = %q", res.ClientKey)
	}
	if gotPath != "/api" {
		t.Errorf("path = %q, want /api", gotPath)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q", gotContentType)
	}
	if gotBody["devicetype"] != "relume-tv#host" {
		t.Errorf("devicetype = %v", gotBody["devicetype"])
	}
	// Omitting generateclientkey would pair successfully but yield no PSK.
	if gotBody["generateclientkey"] != true {
		t.Errorf("generateclientkey = %v, want true", gotBody["generateclientkey"])
	}
}

func TestPair_linkButtonNotPressed(t *testing.T) {
	// Given: the canonical Hue "link button not pressed" rejection
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"error":{"type":101,"address":"","description":"link button not pressed"}}]`))
	}))
	defer srv.Close()

	// When
	res, err := Pair(HTTPClientFor(&config.BridgePro{SkipTLSVerify: true}), proHost(t, srv), "relume-tv#host")

	// Then: the bridge's own wording must reach the user, since it tells them
	// exactly what to do next
	if err == nil {
		t.Fatal("expected an error when the link button was not pressed")
	}
	if !strings.Contains(err.Error(), "link button not pressed") {
		t.Errorf("error should carry the bridge description, got %v", err)
	}
	if res != nil {
		t.Errorf("no result expected on failure, got %+v", res)
	}
}

func TestPair_emptyResponseArray(t *testing.T) {
	// Given: a bridge answering with an empty array
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	// When
	_, err := Pair(HTTPClientFor(&config.BridgePro{SkipTLSVerify: true}), proHost(t, srv), "relume-tv#host")

	// Then
	if err == nil || !strings.Contains(err.Error(), "empty pairing response") {
		t.Fatalf("expected an empty-response error, got %v", err)
	}
}

func TestPair_responseWithNeitherSuccessNorError(t *testing.T) {
	// Given: a well-formed array whose element carries neither key
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"something-else":{}}]`))
	}))
	defer srv.Close()

	// When
	_, err := Pair(HTTPClientFor(&config.BridgePro{SkipTLSVerify: true}), proHost(t, srv), "relume-tv#host")

	// Then: better an explicit error than a PairResult full of empty strings
	if err == nil || !strings.Contains(err.Error(), "unexpected pairing response") {
		t.Fatalf("expected an unexpected-response error, got %v", err)
	}
}

func TestPair_malformedJSON(t *testing.T) {
	// Given: a body that is not the expected array shape
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"not":"an array"}`))
	}))
	defer srv.Close()

	// When
	_, err := Pair(HTTPClientFor(&config.BridgePro{SkipTLSVerify: true}), proHost(t, srv), "relume-tv#host")

	// Then
	if err == nil || !strings.Contains(err.Error(), "parse pairing response") {
		t.Fatalf("expected a parse error, got %v", err)
	}
}

func TestPair_unreachableBridge(t *testing.T) {
	// Given: a server closed before pairing is attempted
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	host := proHost(t, srv)
	srv.Close()

	// When
	_, err := Pair(HTTPClientFor(&config.BridgePro{SkipTLSVerify: true}), host, "relume-tv#host")

	// Then
	if err == nil || !strings.Contains(err.Error(), "pairing request") {
		t.Fatalf("expected a request error, got %v", err)
	}
}

func TestHTTPClientFor_skipVerifyAcceptsAnyCertificate(t *testing.T) {
	// Given: the pre-pairing posture, where no fingerprint is pinned yet
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`ok`))
	}))
	defer srv.Close()

	// When
	resp, err := HTTPClientFor(&config.BridgePro{SkipTLSVerify: true}).Get(srv.URL)

	// Then: the httptest certificate is self-signed, so a verifying client would
	// refuse it
	if err != nil {
		t.Fatalf("skip-verify client rejected a self-signed certificate: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestHTTPClientFor_pinnedClientRejectsTheWrongCertificate(t *testing.T) {
	// Given: a client pinned to a fingerprint that is not the server's
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`ok`))
	}))
	defer srv.Close()
	client := HTTPClientFor(&config.BridgePro{CertSHA256: strings.Repeat("00", 32)})

	// When
	_, err := client.Get(srv.URL)

	// Then: pinning is the only chain check there is, so it must actually bite
	if err == nil {
		t.Fatal("the pinned client accepted a certificate it was not pinned to")
	}
	if !strings.Contains(err.Error(), "fingerprint does not match") {
		t.Errorf("error should name the pin mismatch, got %v", err)
	}
}

func TestHTTPClientFor_pinnedClientAcceptsTheMatchingCertificate(t *testing.T) {
	// Given: a client pinned to the server's actual leaf fingerprint
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`ok`))
	}))
	defer srv.Close()
	client := HTTPClientFor(&config.BridgePro{CertSHA256: pinOf(t, srv)})

	// When
	resp, err := client.Get(srv.URL)

	// Then
	if err != nil {
		t.Fatalf("the pinned client rejected its own pinned certificate: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestHTTPClientFor_noPinAndNoSkipStillSkipsChainVerification(t *testing.T) {
	// Given: neither a pin nor an explicit skip. VerifyPeerCertificate is only
	// installed when a fingerprint exists, and InsecureSkipVerify is always on,
	// so the connection succeeds — documenting the current posture.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`ok`))
	}))
	defer srv.Close()

	// When
	resp, err := HTTPClientFor(&config.BridgePro{}).Get(srv.URL)

	// Then
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

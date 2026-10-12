package bridgepro

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// clipMux serves the given path->body map as a CLIP v2 bridge would, recording
// the hue-application-key header of the last request for auth assertions.
func clipMux(t *testing.T, routes map[string]string, gotKey *string) *httptest.Server {
	t.Helper()
	return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if gotKey != nil {
			*gotKey = r.Header.Get(appKeyHeader)
		}
		body, ok := routes[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"errors":[{"description":"not found"}]}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
}

func TestBridgeInfo_ReadsNameFromOwningDevice(t *testing.T) {
	// Given: a bridge resource whose owner rid resolves to a named device
	var key string
	srv := clipMux(t, map[string]string{
		"/clip/v2/resource/bridge": `{"data":[{"id":"br-1","bridge_id":"001788FFFE123456",
			"owner":{"rid":"dev-9"}}]}`,
		"/clip/v2/resource/device/dev-9": `{"data":[{"id":"dev-9","metadata":{"name":"Wohnzimmer Bridge"}}]}`,
	}, &key)
	defer srv.Close()

	// When
	name, bridgeID, err := pinnedClient(t, srv, pinOf(t, srv)).BridgeInfo()

	// Then
	if err != nil {
		t.Fatalf("BridgeInfo: %v", err)
	}
	if name != "Wohnzimmer Bridge" {
		t.Errorf("name = %q, want %q", name, "Wohnzimmer Bridge")
	}
	if bridgeID != "001788FFFE123456" {
		t.Errorf("bridgeID = %q", bridgeID)
	}
	// The app key must be sent on the CLIP v2 read, or the Pro answers 401.
	if key != "test-app-key" {
		t.Errorf("app key header = %q", key)
	}
}

func TestBridgeInfo_NoOwnerKeepsBridgeIDAndSkipsDeviceLookup(t *testing.T) {
	// Given: a bridge resource without an owner rid — there is no device to name it
	srv := clipMux(t, map[string]string{
		"/clip/v2/resource/bridge": `{"data":[{"id":"br-1","bridge_id":"AABBCCDDEEFF","owner":{"rid":""}}]}`,
	}, nil)
	defer srv.Close()

	// When
	name, bridgeID, err := pinnedClient(t, srv, pinOf(t, srv)).BridgeInfo()

	// Then: the id survives, the name is simply empty (best-effort)
	if err != nil {
		t.Fatalf("BridgeInfo: %v", err)
	}
	if name != "" {
		t.Errorf("name = %q, want empty", name)
	}
	if bridgeID != "AABBCCDDEEFF" {
		t.Errorf("bridgeID = %q", bridgeID)
	}
}

func TestBridgeInfo_DeviceLookupFailureKeepsBridgeID(t *testing.T) {
	// Given: the bridge resource resolves but the device fetch 404s. The name is
	// documented as best-effort, so this must NOT fail the whole call.
	srv := clipMux(t, map[string]string{
		"/clip/v2/resource/bridge": `{"data":[{"id":"br-1","bridge_id":"BEEF0000CAFE","owner":{"rid":"missing"}}]}`,
	}, nil)
	defer srv.Close()

	// When
	name, bridgeID, err := pinnedClient(t, srv, pinOf(t, srv)).BridgeInfo()

	// Then
	if err != nil {
		t.Fatalf("BridgeInfo must tolerate a failing device lookup, got %v", err)
	}
	if name != "" {
		t.Errorf("name = %q, want empty", name)
	}
	if bridgeID != "BEEF0000CAFE" {
		t.Errorf("bridgeID = %q", bridgeID)
	}
}

func TestBridgeInfo_EmptyDeviceListLeavesNameEmpty(t *testing.T) {
	// Given: the device endpoint answers 200 but with an empty data array
	srv := clipMux(t, map[string]string{
		"/clip/v2/resource/bridge":       `{"data":[{"id":"br-1","bridge_id":"1234","owner":{"rid":"dev-0"}}]}`,
		"/clip/v2/resource/device/dev-0": `{"data":[]}`,
	}, nil)
	defer srv.Close()

	// When
	name, bridgeID, err := pinnedClient(t, srv, pinOf(t, srv)).BridgeInfo()

	// Then
	if err != nil || name != "" || bridgeID != "1234" {
		t.Fatalf("got name=%q id=%q err=%v", name, bridgeID, err)
	}
}

func TestBridgeInfo_NoBridgeResourceIsAnError(t *testing.T) {
	// Given: a Pro that returns an empty bridge list — nothing to report
	srv := clipMux(t, map[string]string{
		"/clip/v2/resource/bridge": `{"data":[]}`,
	}, nil)
	defer srv.Close()

	// When
	_, _, err := pinnedClient(t, srv, pinOf(t, srv)).BridgeInfo()

	// Then
	if err == nil {
		t.Fatal("expected an error for an empty bridge list")
	}
	if !strings.Contains(err.Error(), "no bridge resource") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestBridgeInfo_UnreachableBridgeWrapsErrUnreachable(t *testing.T) {
	// Given: a server that is closed before the call, so the round-trip fails
	srv := clipMux(t, map[string]string{}, nil)
	c := pinnedClient(t, srv, pinOf(t, srv))
	srv.Close()

	// When
	_, _, err := c.BridgeInfo()

	// Then: callers switch on ErrUnreachable to decide to re-discover the Pro
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("expected ErrUnreachable, got %v", err)
	}
}

func TestLights_DecodesCapabilitiesAndSortsByID(t *testing.T) {
	// Given: three lights out of order with differing capability sub-objects
	srv := clipMux(t, map[string]string{
		"/clip/v2/resource/light": `{"errors":[],"data":[
			{"id":"c-ct","id_v1":"/lights/3","metadata":{"name":"CT only"},"on":{"on":false},
			 "dimming":{"brightness":40.5},"color_temperature":{"mirek":250},"owner":{"rid":"dev-c"}},
			{"id":"a-color","id_v1":"/lights/1","metadata":{"name":"Colour"},"on":{"on":true},
			 "dimming":{"brightness":100},"color":{"xy":{"x":0.4,"y":0.35}},"owner":{"rid":"dev-a"}},
			{"id":"b-plain","metadata":{"name":"Plug"},"on":{"on":true},"owner":{"rid":"dev-b"}}
		]}`,
	}, nil)
	defer srv.Close()

	// When
	lights, err := pinnedClient(t, srv, pinOf(t, srv)).Lights()

	// Then: stable sort by ID, so the UI ordering does not jump between polls
	if err != nil {
		t.Fatalf("Lights: %v", err)
	}
	if len(lights) != 3 {
		t.Fatalf("got %d lights, want 3", len(lights))
	}
	gotIDs := []string{lights[0].ID, lights[1].ID, lights[2].ID}
	wantIDs := []string{"a-color", "b-plain", "c-ct"}
	for i := range wantIDs {
		if gotIDs[i] != wantIDs[i] {
			t.Fatalf("ids = %v, want %v", gotIDs, wantIDs)
		}
	}

	// The colour light: xy present, no CT.
	col := lights[0]
	if !col.HasColor() || !col.HasDimming() || col.HasColorTemperature() {
		t.Errorf("colour light capabilities: color=%v dim=%v ct=%v",
			col.HasColor(), col.HasDimming(), col.HasColorTemperature())
	}
	if col.Color.XY.X != 0.4 || col.Color.XY.Y != 0.35 {
		t.Errorf("xy = %v/%v", col.Color.XY.X, col.Color.XY.Y)
	}
	if col.Metadata.Name != "Colour" || col.IDv1 != "/lights/1" || !col.On.On {
		t.Errorf("colour light metadata mis-decoded: %+v", col)
	}
	if col.Owner.RID != "dev-a" {
		t.Errorf("owner rid = %q", col.Owner.RID)
	}

	// The plug: an absent key must stay nil, not decode as a zero value — that is
	// how relume-tv tells "cannot do colour" from "colour is currently 0,0".
	plug := lights[1]
	if plug.HasColor() || plug.HasDimming() || plug.HasColorTemperature() {
		t.Errorf("plug should have no capabilities, got %+v", plug)
	}

	// The CT-only light: CT and dimming, no colour.
	ct := lights[2]
	if ct.HasColor() {
		t.Error("CT-only light must not report colour support")
	}
	if !ct.HasColorTemperature() || ct.ColorTemperature.Mirek != 250 {
		t.Errorf("mirek = %+v", ct.ColorTemperature)
	}
	if !ct.HasDimming() || ct.Dimming.Brightness != 40.5 {
		t.Errorf("brightness = %+v", ct.Dimming)
	}
}

func TestLights_EmptyListIsNotAnError(t *testing.T) {
	// Given: a Pro with no lights paired yet
	srv := clipMux(t, map[string]string{
		"/clip/v2/resource/light": `{"errors":[],"data":[]}`,
	}, nil)
	defer srv.Close()

	// When
	lights, err := pinnedClient(t, srv, pinOf(t, srv)).Lights()

	// Then
	if err != nil {
		t.Fatalf("Lights: %v", err)
	}
	if len(lights) != 0 {
		t.Fatalf("got %d lights, want 0", len(lights))
	}
}

func TestLights_QueueFullIsDistinguishable(t *testing.T) {
	// Given: the Pro answers 503 because its command queue is saturated. Callers
	// must back off rather than re-discover, so the class has to survive.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`queue full`))
	}))
	defer srv.Close()

	// When
	_, err := pinnedClient(t, srv, pinOf(t, srv)).Lights()

	// Then
	if !errors.Is(err, ErrQueueFull) {
		t.Fatalf("expected ErrQueueFull, got %v", err)
	}
	if errors.Is(err, ErrUnreachable) {
		t.Error("a 503 means reachable-but-busy; it must not read as unreachable")
	}
}

func TestLights_HTTPErrorSurfacesStatusAndBody(t *testing.T) {
	// Given: an app key the Pro rejects
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`unauthorized user`))
	}))
	defer srv.Close()

	// When
	_, err := pinnedClient(t, srv, pinOf(t, srv)).Lights()

	// Then
	if err == nil {
		t.Fatal("expected an error for 401")
	}
	if !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "unauthorized user") {
		t.Errorf("error should carry status and body, got %v", err)
	}
	if errors.Is(err, ErrQueueFull) || errors.Is(err, ErrUnreachable) {
		t.Errorf("a 401 is neither queue-full nor unreachable: %v", err)
	}
}

func TestLights_MalformedJSONIsAnError(t *testing.T) {
	// Given: a truncated body (e.g. the Pro dropped the connection mid-response)
	srv := clipMux(t, map[string]string{
		"/clip/v2/resource/light": `{"data":[{"id":`,
	}, nil)
	defer srv.Close()

	// When
	_, err := pinnedClient(t, srv, pinOf(t, srv)).Lights()

	// Then
	if err == nil {
		t.Fatal("expected a decode error for malformed JSON")
	}
}

func TestEntertainmentConfigs_DecodesStatusAndSortsByID(t *testing.T) {
	// Given: two configs out of order, one active
	srv := clipMux(t, map[string]string{
		"/clip/v2/resource/entertainment_configuration": `{"errors":[],"data":[
			{"id":"zz","metadata":{"name":"Other app"},"status":"active"},
			{"id":"aa","metadata":{"name":"relume-tv"},"status":"inactive"}
		]}`,
	}, nil)
	defer srv.Close()

	// When
	cfgs, err := pinnedClient(t, srv, pinOf(t, srv)).EntertainmentConfigs()

	// Then
	if err != nil {
		t.Fatalf("EntertainmentConfigs: %v", err)
	}
	if len(cfgs) != 2 {
		t.Fatalf("got %d configs, want 2", len(cfgs))
	}
	if cfgs[0].ID != "aa" || cfgs[1].ID != "zz" {
		t.Fatalf("not sorted by id: %q, %q", cfgs[0].ID, cfgs[1].ID)
	}
	if cfgs[0].Metadata.Name != "relume-tv" || cfgs[0].Status != "inactive" {
		t.Errorf("config 0 = %+v", cfgs[0])
	}
	// A config already streamed by another app reports "active" — the streamer
	// relies on this to detect that it must take the channel over.
	if cfgs[1].Status != "active" {
		t.Errorf("config 1 status = %q, want active", cfgs[1].Status)
	}
}

func TestEntertainmentConfigs_UnreachableWrapsErrUnreachable(t *testing.T) {
	// Given: a server closed before the call
	srv := clipMux(t, map[string]string{}, nil)
	c := pinnedClient(t, srv, pinOf(t, srv))
	srv.Close()

	// When
	_, err := c.EntertainmentConfigs()

	// Then
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("expected ErrUnreachable, got %v", err)
	}
}

func TestLights_CertificatePinMismatchIsUnreachable(t *testing.T) {
	// Given: a client pinned to the wrong leaf fingerprint. A mismatched pin is a
	// TLS handshake failure, which the taxonomy classes as unreachable.
	srv := clipMux(t, map[string]string{
		"/clip/v2/resource/light": `{"data":[]}`,
	}, nil)
	defer srv.Close()
	wrongPin := strings.Repeat("ab", 32)

	// When
	_, err := pinnedClient(t, srv, wrongPin).Lights()

	// Then
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("expected ErrUnreachable for a pin mismatch, got %v", err)
	}
	if !strings.Contains(err.Error(), "fingerprint does not match") {
		t.Errorf("error should explain the pin mismatch, got %v", err)
	}
}

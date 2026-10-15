package bridge

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/trick77/relume-tv/internal/bridgepro"
	"github.com/trick77/relume-tv/internal/clipv1"
)

// downClient models an unreachable Hue Bridge Pro: every write fails, so the provider's
// forward-error path (and the OnForwardErr UI callback) is exercised.
type downClient struct{}

func (downClient) Lights() ([]bridgepro.Light, error)    { return nil, nil }
func (downClient) SetLight(string, map[string]any) error { return errors.New("pro unreachable") }

// fakeClient records the v2 bodies forwarded to the Hue Bridge Pro. The first
// SetLight call blocks on gate (if set), letting a test hold the drain goroutine
// busy while it queues more writes — to observe coalescing deterministically.
type fakeClient struct {
	mu    sync.Mutex
	mirek []int
	gate  chan struct{}
	n     int
}

func (f *fakeClient) Lights() ([]bridgepro.Light, error) { return nil, nil }

func (f *fakeClient) SetLight(_ string, body map[string]any) error {
	f.mu.Lock()
	first := f.n == 0
	f.n++
	f.mu.Unlock()
	if first && f.gate != nil {
		<-f.gate
	}
	ct, _ := body["color_temperature"].(map[string]any)
	f.mu.Lock()
	f.mirek = append(f.mirek, ct["mirek"].(int))
	f.mu.Unlock()
	return nil
}

func (f *fakeClient) seen() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.mirek...)
}

func (f *fakeClient) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.n
}

func newTestProvider(c proClient) *LightProvider {
	p := &LightProvider{client: c, pending: map[string]map[string]any{}}
	p.v1ToUUID = map[string]string{"1": "uuid-1", "2": "uuid-2"} // skip the Lights() resolution
	p.uuidToV1 = map[string]string{"uuid-1": "1", "uuid-2": "2"} // inverse, as LightsV1 builds it
	return p
}

func TestV1ForUUID_isInverseOfUUIDForV1(t *testing.T) {
	p := newTestProvider(&fakeClient{})

	if v1, ok := p.V1ForUUID("uuid-2"); !ok || v1 != "2" {
		t.Fatalf("V1ForUUID(uuid-2) = %q,%v want 2,true", v1, ok)
	}
	// Round-trips with the forward map.
	if uuid, _ := p.UUIDForV1("1"); uuid != "uuid-1" {
		t.Fatalf("UUIDForV1(1) = %q want uuid-1", uuid)
	}
	if _, ok := p.V1ForUUID("uuid-unknown"); ok {
		t.Fatalf("V1ForUUID(unknown) ok = true, want false")
	}
}

func TestOnControlled_firesOnEveryForward(t *testing.T) {
	// Given: a provider that reports which UUIDs the TV drives
	fc := &fakeClient{}
	p := newTestProvider(fc)
	var mu sync.Mutex
	var got []string
	p.OnControlled = func(uuid string) {
		mu.Lock()
		got = append(got, uuid)
		mu.Unlock()
	}

	// When: light 1 is driven twice and light 2 once
	_ = p.forward("1", map[string]any{"ct": 200})
	_ = p.forward("1", map[string]any{"ct": 200})
	_ = p.forward("2", map[string]any{"ct": 200})

	// Then: it fires on every forward (so the sliding window keeps being refreshed)
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 3 || got[0] != "uuid-1" || got[1] != "uuid-1" || got[2] != "uuid-2" {
		t.Fatalf("OnControlled calls = %v, want [uuid-1 uuid-1 uuid-2]", got)
	}
}

func TestForward_emptyV2StateSkipsWriteAndControlled(t *testing.T) {
	// Given: a provider that reports which UUIDs the TV drives
	fc := &fakeClient{}
	p := newTestProvider(fc)
	var mu sync.Mutex
	var controlled int
	p.OnControlled = func(string) {
		mu.Lock()
		controlled++
		mu.Unlock()
	}

	// When: a state that translates to an empty v2 body (e.g. a group action carrying
	// only non-light-state keys like "scene", which StateV1ToV2 drops)
	if err := p.forward("1", map[string]any{"scene": "abc"}); err != nil {
		t.Fatalf("forward: %v", err)
	}

	// Then: nothing is written to the Pro and the light is NOT marked controlled
	mu.Lock()
	gotControlled := controlled
	mu.Unlock()
	if fc.calls() != 0 {
		t.Errorf("SetLight calls = %d, want 0 for an empty-yielding state", fc.calls())
	}
	if gotControlled != 0 {
		t.Errorf("OnControlled calls = %d, want 0 for an empty-yielding state", gotControlled)
	}

	// And: a real state still forwards and marks controlled
	if err := p.forward("1", map[string]any{"ct": 200}); err != nil {
		t.Fatalf("forward (real): %v", err)
	}
	mu.Lock()
	gotControlled = controlled
	mu.Unlock()
	if fc.calls() != 1 || gotControlled != 1 {
		t.Errorf("after real state: SetLight=%d controlled=%d, want 1/1", fc.calls(), gotControlled)
	}
}

func TestForward_failedWriteDoesNotMarkControlled(t *testing.T) {
	// Given: a provider whose Pro write always fails (down/overloaded)
	p := newTestProvider(downClient{})
	var controlled, colored int
	p.OnControlled = func(string) { controlled++ }
	p.OnColor = func(string, map[string]any) { colored++ }

	// When: a real state is forwarded but the Pro rejects the write
	err := p.forward("1", map[string]any{"ct": 200})

	// Then: the error propagates and the light is NEITHER marked controlled (so the
	// restart/idle turn-off won't target a bulb the TV never actually drove) NOR surfaced to
	// the UI as a live colour it never received.
	if err == nil {
		t.Fatal("forward: expected an error from a failed write, got nil")
	}
	if controlled != 0 {
		t.Errorf("OnControlled calls = %d, want 0 on a failed write", controlled)
	}
	if colored != 0 {
		t.Errorf("OnColor calls = %d, want 0 on a failed write", colored)
	}
}

func TestSetLightV1_isAsyncAndForwards(t *testing.T) {
	// Given: a provider over a fake Hue Bridge Pro
	fc := &fakeClient{}
	p := newTestProvider(fc)

	// When: a single write
	if err := p.SetLightV1("1", map[string]any{"ct": 200}); err != nil {
		t.Fatalf("SetLightV1 returned error: %v", err)
	}

	// Then: it is eventually forwarded to the Hue Bridge Pro (asynchronously)
	waitFor(t, func() bool { return len(fc.seen()) == 1 })
	if got := fc.seen(); got[0] != 200 {
		t.Fatalf("forwarded mirek = %v, want [200]", got)
	}
}

func TestSetLightV1_coalescesToLatestPerLight(t *testing.T) {
	// Given: the first forward is held in flight so writes pile up behind it
	gate := make(chan struct{})
	fc := &fakeClient{gate: gate}
	p := newTestProvider(fc)

	// When: the first write starts the drain and blocks inside SetLight
	p.SetLightV1("1", map[string]any{"ct": 1})
	waitFor(t, func() bool {
		fc.mu.Lock()
		defer fc.mu.Unlock()
		return fc.n == 1 // drain goroutine is now blocked in the first SetLight
	})
	// ...and several more states queue while the Hue Bridge Pro is busy
	for _, v := range []int{2, 3, 4, 5} {
		p.SetLightV1("1", map[string]any{"ct": v})
	}

	// Then: releasing the gate forwards only the first and the latest — the
	// intermediate frames (2,3,4) are coalesced away.
	close(gate)
	waitFor(t, func() bool { return len(fc.seen()) == 2 })
	got := fc.seen()
	if len(got) != 2 || got[0] != 1 || got[1] != 5 {
		t.Fatalf("forwarded mirek = %v, want [1 5] (intermediate frames coalesced)", got)
	}
}

// OnCoalesce fires once per frame the optimistic path drops (a newer state for the
// same light arriving before the Pro accepted the previous one) — the drops/s the
// Backpressure card shows. Mirrors the coalescing test, but counts the callback.
func TestSetLightV1_firesOnCoalesce(t *testing.T) {
	// Given: the first forward is held in flight so writes pile up behind it
	gate := make(chan struct{})
	fc := &fakeClient{gate: gate}
	p := newTestProvider(fc)
	var coalesced atomic.Int64
	p.OnCoalesce = func() { coalesced.Add(1) }

	// When: the first write blocks the drain, then more states for the SAME light queue
	p.SetLightV1("1", map[string]any{"ct": 1})
	waitFor(t, func() bool {
		fc.mu.Lock()
		defer fc.mu.Unlock()
		return fc.n == 1 // drain is now blocked inside the first SetLight
	})
	for _, v := range []int{2, 3, 4, 5} {
		p.SetLightV1("1", map[string]any{"ct": v})
	}

	// Then: write 2 found the queue empty (drained), writes 3,4,5 each replaced a
	// still-pending state → three coalesced drops.
	if got := coalesced.Load(); got != 3 {
		t.Fatalf("OnCoalesce fired %d times, want 3 (one per dropped frame)", got)
	}
	close(gate) // let the drain goroutine finish
}

// OnForwardErr fires once per failed REST write to the Pro — the cumulative error
// count the Backpressure card shows (distinct from the healthy coalesce drops).
func TestSetLightV1_firesOnForwardErr(t *testing.T) {
	// Given: a provider whose every write to the Pro fails (Pro unreachable)
	p := newTestProvider(downClient{})
	var errs atomic.Int64
	p.OnForwardErr = func() { errs.Add(1) }

	// When: a write is queued and the async drain tries (and fails) to forward it
	if err := p.SetLightV1("1", map[string]any{"ct": 200}); err != nil {
		t.Fatalf("SetLightV1 returned error: %v", err)
	}

	// Then: OnForwardErr fires for the failed Pro write
	waitFor(t, func() bool { return errs.Load() == 1 })
}

// Close is the shutdown fence: the write in flight finishes before it returns, the
// queued ones and any later one never reach the Pro, so nothing lands behind the
// lights-off.
func TestClose_waitsForTheWriteInFlightAndDropsTheRest(t *testing.T) {
	// Given: one write held in flight at the Pro, two more queued behind it
	gate := make(chan struct{})
	fc := &fakeClient{gate: gate}
	p := newTestProvider(fc)
	p.SetLightV1("1", map[string]any{"ct": 1})
	waitFor(t, func() bool { return fc.calls() == 1 })
	p.SetLightV1("1", map[string]any{"ct": 2})
	p.SetLightV1("2", map[string]any{"ct": 3})

	// When: Close runs while the write is still in flight
	if p.Close(10 * time.Millisecond) {
		t.Fatal("Close = true while a write is still in flight")
	}
	closed := make(chan bool, 1)
	go func() { closed <- p.Close(2 * time.Second) }()
	select {
	case <-closed:
		t.Fatal("Close returned before the write in flight finished")
	case <-time.After(20 * time.Millisecond):
	}
	close(gate)

	// Then: it returns once that write is done, and nothing else is forwarded
	if !<-closed {
		t.Fatal("Close = false after the write in flight finished")
	}
	p.SetLightV1("1", map[string]any{"ct": 4}) // dropped synchronously: no drain starts
	if got := fc.seen(); len(got) != 1 || got[0] != 1 {
		t.Fatalf("forwarded mirek = %v, want [1] (queued and later writes dropped)", got)
	}
	if !p.Close(time.Millisecond) {
		t.Fatal("Close on an idle provider = false")
	}
}

// clipv1's shutdown fence finds Close through this interface.
var _ clipv1.LightWriteCloser = (*LightProvider)(nil)

// The rest of a batch already taken by the drain is dropped once Close has run.
func TestClose_dropsTheRestOfATakenBatch(t *testing.T) {
	// Given: a drain holding a two-light batch, its first write in flight
	gate := make(chan struct{})
	fc := &fakeClient{gate: gate}
	p := newTestProvider(fc)
	p.pending = map[string]map[string]any{"1": {"ct": 1}, "2": {"ct": 2}}
	p.drained = make(chan struct{})
	go p.drain(p.drained)
	waitFor(t, func() bool { return fc.calls() == 1 })

	// When: Close runs, then the write in flight finishes
	p.Close(0)
	close(gate)
	if !p.Close(2 * time.Second) {
		t.Fatal("drain did not finish")
	}

	// Then: only that write reached the Pro
	if got := fc.seen(); len(got) != 1 {
		t.Fatalf("forwarded mirek = %v, want one write", got)
	}
}

// A write whose light-list fetch outlasts the fence must not reach the Pro after it.
func TestClose_dropsAWriteWhoseFetchOutlastsIt(t *testing.T) {
	// Given: a fresh provider (no id mapping) whose first write waits on the list fetch
	gate := make(chan struct{})
	lc := &lightsClient{lights: []bridgepro.Light{colorLight("uuid-a")}, gate: gate}
	p := &LightProvider{client: lc, pending: map[string]map[string]any{}}
	p.SetLightV1("1", map[string]any{"ct": 200})
	waitFor(t, func() bool { return lc.n() == 1 })

	// When: the fence runs out while the fetch is in flight, then the fetch returns
	if p.Close(0) {
		t.Fatal("Close = true with the fetch in flight")
	}
	close(gate)
	if !p.Close(2 * time.Second) {
		t.Fatal("drain did not finish")
	}

	// Then: the light resolved, yet nothing was written
	if _, ok := p.UUIDForV1("1"); !ok {
		t.Fatal("light 1 did not resolve: the test would pass vacuously")
	}
	lc.mu.Lock()
	defer lc.mu.Unlock()
	if lc.writes != 0 {
		t.Fatalf("SetLight calls = %d after Close, want 0", lc.writes)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not met within deadline")
}

// lightsClient serves a scripted Lights() result. Each call blocks on gate (if
// set) so a test can observe what other callers do while a Pro fetch is in flight.
type lightsClient struct {
	mu     sync.Mutex
	lights []bridgepro.Light
	err    error
	gate   chan struct{}
	calls  int
	writes int
}

func (c *lightsClient) Lights() ([]bridgepro.Light, error) {
	c.mu.Lock()
	c.calls++
	gate, lights, err := c.gate, c.lights, c.err
	c.mu.Unlock()
	if gate != nil {
		<-gate
	}
	return lights, err
}

func (c *lightsClient) SetLight(string, map[string]any) error {
	c.mu.Lock()
	c.writes++
	c.mu.Unlock()
	return nil
}

func (c *lightsClient) n() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func colorLight(id string) bridgepro.Light {
	var l bridgepro.Light
	l.ID = id
	l.Color = &bridgepro.LightColor{}
	return l
}

func TestLightsV1_lookupsDoNotWaitOnAnInFlightProFetch(t *testing.T) {
	// Given: a Pro whose Lights() call hangs, and a provider with a known mapping
	gate := make(chan struct{})
	c := &lightsClient{lights: []bridgepro.Light{colorLight("uuid-1")}, gate: gate}
	p := newTestProvider(c)

	// When: a TV poll triggers the (hanging) fetch
	fetched := make(chan struct{})
	go func() { defer close(fetched); _, _ = p.LightsV1() }()
	for c.n() == 0 {
		time.Sleep(time.Millisecond)
	}

	// Then: id lookups (the DTLS path) return immediately while the fetch hangs
	done := make(chan struct{})
	go func() { defer close(done); p.UUIDForV1("1"); p.V1ForUUID("uuid-1") }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("UUIDForV1/V1ForUUID blocked behind the Pro fetch")
	}
	close(gate)
	<-fetched
}

func TestLightsV1_concurrentPollsShareOneProFetch(t *testing.T) {
	// Given: a hanging Pro fetch
	gate := make(chan struct{})
	c := &lightsClient{lights: []bridgepro.Light{colorLight("uuid-1")}, gate: gate}
	p := newTestProvider(c)

	// When: three TV polls arrive while the first fetch is in flight
	var wg sync.WaitGroup
	results := make([]map[string]any, 3)
	for i := range results {
		wg.Add(1)
		go func() { defer wg.Done(); results[i], _ = p.LightsV1() }()
	}
	for c.n() == 0 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond) // let the other two reach the wait
	close(gate)
	wg.Wait()

	// Then: one Pro request, every caller got the list
	if c.n() != 1 {
		t.Errorf("Pro Lights() calls = %d; want 1", c.n())
	}
	for i, r := range results {
		if _, ok := r["1"]; !ok {
			t.Errorf("caller %d got %v; want light 1", i, r)
		}
	}
}

func TestLightsV1_servesLastKnownListWhenRefreshFails(t *testing.T) {
	// Given: one successful fetch, then the Pro goes away and the cache expires
	c := &lightsClient{lights: []bridgepro.Light{colorLight("uuid-1")}}
	p := newTestProvider(c)
	if _, err := p.LightsV1(); err != nil {
		t.Fatalf("first LightsV1: %v", err)
	}
	c.mu.Lock()
	c.err = errors.New("pro unreachable")
	c.mu.Unlock()
	p.mu.Lock()
	p.fetchedAt = time.Time{}
	p.mu.Unlock()

	// When
	got, err := p.LightsV1()

	// Then: the TV still gets the last known list, no error
	if err != nil {
		t.Fatalf("LightsV1 after Pro failure: %v", err)
	}
	if _, ok := got["1"]; !ok {
		t.Errorf("got %v; want the cached light 1", got)
	}
	// A background refresh was attempted, and its failure pushes the next retry a
	// TTL out instead of firing on the very next poll.
	waitFor(t, func() bool { return c.n() == 2 })
	waitFor(t, func() bool { p.mu.Lock(); defer p.mu.Unlock(); return p.refresh == nil })
	if _, err := p.LightsV1(); err != nil {
		t.Fatalf("LightsV1 after failed refresh: %v", err)
	}
	if c.n() != 2 {
		t.Errorf("Pro Lights() calls = %d; want still 2 (no retry within the TTL)", c.n())
	}
	if _, ok := p.CachedLightsV1(); !ok {
		t.Error("CachedLightsV1 should report the last known list")
	}
}

func TestLightsV1_staleCacheIsServedWithoutWaitingOnTheRefresh(t *testing.T) {
	// Given: a fetched list, an expired TTL, and a Pro that now hangs
	c := &lightsClient{lights: []bridgepro.Light{colorLight("uuid-1")}}
	p := newTestProvider(c)
	if _, err := p.LightsV1(); err != nil {
		t.Fatalf("first LightsV1: %v", err)
	}
	gate := make(chan struct{})
	c.mu.Lock()
	c.gate = gate
	c.mu.Unlock()
	p.mu.Lock()
	p.fetchedAt = time.Time{}
	p.mu.Unlock()

	// When: a TV poll arrives past the TTL
	done := make(chan struct{})
	go func() { defer close(done); _, _ = p.LightsV1() }()

	// Then: it returns at once (the refresh runs in the background)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("LightsV1 waited on the Pro although a cached list existed")
	}
	waitFor(t, func() bool { return c.n() == 2 })
	close(gate)
}

func TestLightsV1_errorsWhenNeverFetched(t *testing.T) {
	// Given: the Pro has never answered
	c := &lightsClient{err: errors.New("pro unreachable")}
	p := newTestProvider(c)

	// When / Then
	if _, err := p.LightsV1(); err == nil {
		t.Fatal("expected an error with no cache to fall back on")
	}
}

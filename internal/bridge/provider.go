// Package bridge wires the TV-side frontend (clipv1) to the
// Bridge-Pro-side backend (bridgepro) and holds the light mapping.
package bridge

import (
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/trick77/relume-tv/internal/bridgepro"
	"github.com/trick77/relume-tv/internal/translate"
)

// lightCacheTTL limits how often the Hue Bridge Pro is queried for lights.
const lightCacheTTL = 5 * time.Second

// proClient is the Pro read + control surface the provider needs. It aliases the
// canonical bridgepro.ProController (defined in the producer package) so the
// optimistic control path can be tested without a live Hue Bridge Pro.
type proClient = bridgepro.ProController

// LightProvider implements clipv1.LightProvider on top of the Hue Bridge Pro and
// holds the v1→UUID mapping for control (REST fallback path).
type LightProvider struct {
	client proClient
	log    *slog.Logger

	// OnControlled, if set, is called with the Hue Bridge Pro UUID of each light the TV
	// drives, on every per-light forward. It feeds the sliding-window ControlledSet
	// so the restart/idle turn-off touches only the bulbs the TV is
	// currently driving, not the whole home. Wired by main.
	OnControlled func(uuid string)

	// OnColor, if set, is called with each forwarded light's v1 id and its v1 state
	// ({on,bri,xy}) on every per-light forward, so the web UI can show the live
	// streamed colour. Covers the REST-follow and REST-fallback paths; the DTLS
	// passthrough is captured separately in the entertainment streamer. Wired by main.
	OnColor func(v1id string, state map[string]any)

	// OnForward, if set, is called once per light state successfully written to the
	// Hue Bridge Pro over REST (coalesced; dropped frames never reach the Pro and are not
	// counted). Lets the web UI show the live relume-tv→Pro REST write rate — the
	// REST-path counterpart to the DTLS sendLoop's frame rate. Wired by main.
	OnForward func()

	// OnCoalesce, if set, is called once per frame DROPPED because a newer state for
	// the same light arrived before the Hue Bridge Pro accepted the previous one — the
	// optimistic path sparing the Pro a write it could not keep up with. This is
	// healthy backpressure, NOT an error. Fed to the web UI as a drops/s rate. Wired
	// by main.
	OnCoalesce func()

	// OnForwardErr, if set, is called once per FAILED REST write to the Hue Bridge Pro (a
	// down/unreachable Pro, a 503 overflow). Unlike OnCoalesce this is the real error
	// signal. Fed to the web UI as a cumulative count. Wired by main.
	OnForwardErr func()

	mu        sync.Mutex
	cached    map[string]any
	v1ToUUID  map[string]string
	uuidToV1  map[string]string
	fetchedAt time.Time
	// refresh is non-nil while a Pro fetch is in flight (closed when it finishes), so
	// concurrent callers past the TTL wait for it instead of starting their own.
	// lastErr is that fetch's result, for waiters that find no cache to serve.
	refresh      chan struct{}
	lastErr      error
	lastStaleLog time.Time

	// Optimistic REST control: the TV's per-light writes are acknowledged
	// immediately (see SetLightV1) and forwarded to the Hue Bridge Pro asynchronously
	// by drain. pending keeps only the latest state per light, so intermediate
	// Ambilight frames the Hue Bridge Pro cannot keep up with are coalesced away.
	ctrlMu   sync.Mutex
	pending  map[string]map[string]any
	draining bool

	// Forward errors are summarized rather than logged per failed write: the
	// optimistic ack removes the old per-write back-pressure, so a down Hue Bridge Pro
	// would otherwise spam a Warn many times per second (cf. the Ambilight activity
	// summary). errCount accumulates failures between rollups.
	errMu      sync.Mutex
	errCount   int
	lastErrLog time.Time

	// Window stats for the activity rollup, reset by DrainStatsDelta. coalesced
	// counts frames dropped because a newer state for the same light arrived before
	// the Hue Bridge Pro accepted the previous one (the Pro can't keep up); forwardErr
	// counts failed writes to the Pro.
	coalesced  atomic.Uint64
	forwardErr atomic.Uint64
}

// DrainStatsDelta returns the coalesced (dropped) frame count and the forward
// error count since the last call, and resets both. Used by the activity rollup.
func (p *LightProvider) DrainStatsDelta() (coalesced, forwardErrors uint64) {
	return p.coalesced.Swap(0), p.forwardErr.Swap(0)
}

// errLogInterval bounds how often forward failures are logged (a summary with the
// suppressed count), matching the Ambilight activity-summary cadence.
const errLogInterval = 30 * time.Second

// NewLightProvider creates a provider for the given Hue Bridge Pro. log receives the
// asynchronous control-path errors that are no longer surfaced to the TV.
func NewLightProvider(client *bridgepro.Client, log *slog.Logger) *LightProvider {
	return &LightProvider{client: client, log: log, pending: map[string]map[string]any{}}
}

// LightsV1 returns the v1 light list (with a short cache).
//
// The Hue Bridge Pro round-trip (up to the client's 10 s timeout) never runs under
// p.mu: the TV polls GET /lights/{id} at a few Hz and a slow or unreachable Pro must
// not stall those polls behind a lock — nor the id lookups the DTLS path needs.
// Concurrent callers past the TTL share one fetch. When a refresh fails but an
// earlier one succeeded, the last known list is served (the TV keeps a consistent
// light set while the Pro is briefly away); an error surfaces only when there has
// never been a successful fetch.
func (p *LightProvider) LightsV1() (map[string]any, error) {
	p.mu.Lock()
	if p.cached != nil && time.Since(p.fetchedAt) < lightCacheTTL {
		cached := p.cached
		p.mu.Unlock()
		return cached, nil
	}
	if p.refresh != nil {
		// A refresh is in flight: wait for it rather than piling a second Pro request
		// on top, then serve whatever it produced.
		done := p.refresh
		p.mu.Unlock()
		<-done
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.cached != nil {
			return p.cached, nil
		}
		return nil, p.lastErr
	}
	done := make(chan struct{})
	p.refresh = done
	p.mu.Unlock()

	lights, err := p.client.Lights()

	p.mu.Lock()
	defer p.mu.Unlock()
	p.refresh = nil
	p.lastErr = err
	close(done)
	if err != nil {
		if p.cached != nil {
			if p.log != nil && time.Since(p.lastStaleLog) >= errLogInterval {
				p.lastStaleLog = time.Now()
				p.log.Warn("reading lights from hue bridge pro failed, serving the last known list", "err", err)
			}
			return p.cached, nil
		}
		return nil, err
	}
	lm := translate.LightsV1(lights)
	p.cached = lm.V1
	p.v1ToUUID = lm.V1ToUUID
	p.uuidToV1 = make(map[string]string, len(lm.V1ToUUID))
	for v1, uuid := range lm.V1ToUUID {
		p.uuidToV1[uuid] = v1
	}
	p.fetchedAt = time.Now()
	return p.cached, nil
}

// UUIDForV1 returns the v2 UUID for a numeric v1 light ID.
func (p *LightProvider) UUIDForV1(v1id string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	uuid, ok := p.v1ToUUID[v1id]
	return uuid, ok
}

// V1ForUUID returns the numeric v1 light ID for a v2 resource UUID — the inverse of
// UUIDForV1. Used to intersect a turn-off target (a list of Pro UUIDs) with the TV's
// current Ambilight membership, which is keyed by v1 id.
func (p *LightProvider) V1ForUUID(uuid string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	v1, ok := p.uuidToV1[uuid]
	return v1, ok
}

// SetLightV1 queues the latest state for a light and returns immediately; the
// write is forwarded to the Hue Bridge Pro asynchronously by drain. Acknowledging the
// TV right away keeps its REST control path from blocking on the Hue Bridge Pro
// round-trip — the dominant Ambilight lag. Hue Bridge Pro errors are logged, not
// surfaced to the TV (latency over error reporting). Always returns nil.
func (p *LightProvider) SetLightV1(v1id string, v1state map[string]any) error {
	p.ctrlMu.Lock()
	var coalesced bool
	if _, exists := p.pending[v1id]; exists {
		// A previous frame for this light is still queued → it is dropped (coalesced)
		// because the Hue Bridge Pro has not drained it yet.
		p.coalesced.Add(1)
		coalesced = true
	}
	p.pending[v1id] = v1state
	if !p.draining {
		p.draining = true
		go p.drain()
	}
	p.ctrlMu.Unlock()
	// Fire the UI callback outside the lock (like OnForward), so a slow consumer
	// never stalls the TV's control path.
	if coalesced && p.OnCoalesce != nil {
		p.OnCoalesce()
	}
	return nil
}

// drain forwards queued light states to the Hue Bridge Pro until none remain, keeping
// only the latest state per light. It runs in its own goroutine started by
// SetLightV1 and exits once the queue is empty, so a replaced provider's goroutine
// terminates on its own (no writes arrive → drain finishes).
func (p *LightProvider) drain() {
	for {
		p.ctrlMu.Lock()
		if len(p.pending) == 0 {
			p.draining = false
			p.ctrlMu.Unlock()
			return
		}
		batch := p.pending
		p.pending = map[string]map[string]any{}
		p.ctrlMu.Unlock()

		for v1id, v1state := range batch {
			if err := p.forward(v1id, v1state); err != nil {
				p.recordForwardErr(err)
			}
		}
	}
}

// recordForwardErr logs the first failure of a burst immediately, then suppresses
// further ones and emits a summary at most every errLogInterval with the count of
// suppressed failures — so a down Hue Bridge Pro cannot flood the log.
func (p *LightProvider) recordForwardErr(err error) {
	p.forwardErr.Add(1)
	if p.OnForwardErr != nil {
		p.OnForwardErr()
	}
	if p.log == nil {
		return
	}
	p.errMu.Lock()
	p.errCount++
	now := time.Now()
	if p.lastErrLog.IsZero() || now.Sub(p.lastErrLog) >= errLogInterval {
		count := p.errCount
		p.errCount = 0
		p.lastErrLog = now
		p.errMu.Unlock()
		p.log.Warn("forwarding lights to hue bridge pro failing", "failures", count, "last_err", err)
		return
	}
	p.errMu.Unlock()
}

// forward translates a v1 state to v2 and writes it to the Hue Bridge Pro, resolving
// the v1→UUID mapping (loading the light list once if it is not built yet).
func (p *LightProvider) forward(v1id string, v1state map[string]any) error {
	// Translate first and skip a no-op write: a state that yields an empty v2 body
	// (e.g. a group action carrying only non-light-state keys like "scene", which
	// StateV1ToV2 drops) must not reach the Pro — and must NOT mark the light as
	// controlled. Marking it would taint the controlled set (the restart/idle turn-off
	// targets it) for a light that was never actually driven. The target TV does not
	// send empty-yielding states on the per-light REST path, so this only ever fires
	// for the group-action fan-out and is otherwise inert.
	v2 := translate.StateV1ToV2(v1state)
	if len(v2) == 0 {
		return nil
	}
	uuid, ok := p.UUIDForV1(v1id)
	if !ok {
		// Mapping may not be built yet → load lights once and check again.
		if _, err := p.LightsV1(); err != nil {
			return err
		}
		if uuid, ok = p.UUIDForV1(v1id); !ok {
			return fmt.Errorf("unknown light id %q", v1id)
		}
	}
	// Write to the Pro FIRST, then mark the light driven. Marking it before the write
	// would taint the ControlledSet (the restart/idle turn-off targets it) and surface a UI
	// colour for a light that a down/overloaded Pro never actually received — so a failed
	// write must leave both untouched.
	if err := p.client.SetLight(uuid, v2); err != nil {
		return err
	}
	if p.OnControlled != nil {
		p.OnControlled(uuid)
	}
	if p.OnColor != nil {
		p.OnColor(v1id, v1state)
	}
	if p.OnForward != nil {
		p.OnForward()
	}
	return nil
}

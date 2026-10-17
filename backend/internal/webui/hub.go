// Package webui provides relume-tv's web UI: a guided setup assistant and a live
// status dashboard. It is on by default (disabled with -headless) and never
// touches the TV- or Pro-facing control paths.
package webui

import (
	"sync"

	"github.com/trick77/ship/kit/sse"
)

// Event is a single human-readable log line surfaced to the UI's live event tail.
type Event struct {
	Time  string `json:"time"`  // RFC3339, set by the caller
	Level string `json:"level"` // e.g. "INFO", "WARN"
	Msg   string `json:"msg"`
	Attrs string `json:"attrs,omitempty"` // pre-formatted logfmt, e.g. `failures=3 last_err="i/o timeout"`
}

// Frame is one message pushed over the SSE stream: either a fresh snapshot or a
// single new event.
type Frame struct {
	Kind     string    `json:"kind"` // "snapshot" | "event"
	Snapshot *Snapshot `json:"snapshot,omitempty"`
	Event    *Event    `json:"event,omitempty"`
}

// Hub holds the latest snapshot and a bounded tail of recent events, and fans
// both out to SSE subscribers. All methods are safe for concurrent use.
type Hub struct {
	mu       sync.Mutex
	ringSize int
	events   []Event
	snap     Snapshot
	fan      *sse.Hub[Frame]
}

// subBuffer is each SSE subscriber's frame queue depth.
const subBuffer = 32

// NewHub creates a hub whose event ring keeps at most ringSize recent events.
func NewHub(ringSize int) *Hub {
	if ringSize < 1 {
		ringSize = 1
	}
	return &Hub{ringSize: ringSize, fan: sse.NewHub[Frame](sse.HubOptions{Buffer: subBuffer})}
}

// PublishEvent appends an event to the bounded ring and fans it out to subscribers.
// An event is an append log: a subscriber whose buffer is full simply misses it.
//
// The fan-out runs with h.mu released. The process-wide logger funnels every slog
// line into PublishEvent, so fanning out while holding h.mu would self-deadlock the
// moment any code on the send path logged; kit's hub logs nothing under its own lock.
func (h *Hub) PublishEvent(e Event) {
	h.mu.Lock()
	h.events = append(h.events, e)
	if len(h.events) > h.ringSize {
		h.events = h.events[len(h.events)-h.ringSize:]
	}
	h.mu.Unlock()
	h.fan.Publish(Frame{Kind: "event", Event: &e})
}

// SetSnapshot stores the latest snapshot and fans it out to subscribers, outside
// h.mu as in PublishEvent. A snapshot is coalescible state: a full subscriber's
// oldest queued frame is evicted so the freshest snapshot still lands (a
// briefly-stalled dashboard would otherwise stay on stale state).
func (h *Hub) SetSnapshot(s Snapshot) {
	h.mu.Lock()
	h.snap = s
	h.mu.Unlock()
	h.fan.PublishLatest(Frame{Kind: "snapshot", Snapshot: &s})
}

// Snapshot returns the most recent snapshot (zero value until the first SetSnapshot).
func (h *Hub) Snapshot() Snapshot {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.snap
}

// hasSubscribers reports whether any SSE client is currently connected. The
// snapshot loop uses this to avoid polling state (and the Hue Bridge Pro) when no
// browser is watching.
func (h *Hub) hasSubscribers() bool {
	return h.fan.Len() > 0
}

// Events returns a copy of the buffered events, oldest first.
func (h *Hub) Events() []Event {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]Event, len(h.events))
	copy(out, h.events)
	return out
}

// Subscribe registers a subscriber and returns its frame channel plus a cancel
// func that removes and closes it. ok is false once Close ran (shutdown).
func (h *Hub) Subscribe() (ch <-chan Frame, cancel func(), ok bool) {
	return h.fan.Subscribe("")
}

// Close ends every subscription, so open SSE streams return instead of holding
// the HTTP server's shutdown drain. Later publishes still fill the ring and the
// snapshot; they just reach nobody.
func (h *Hub) Close() {
	h.fan.Close()
}

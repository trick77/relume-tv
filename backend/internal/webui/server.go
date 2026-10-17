package webui

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/trick77/ship/kit/httpx"
	"github.com/trick77/ship/kit/spa"
	"github.com/trick77/ship/kit/sse"

	"github.com/trick77/relume-tv/web"
)

// Server is the web UI HTTP server. It is separate from the TV-facing clipv1
// server and runs by default (suppressed by -headless).
type Server struct {
	addr         string
	hub          *Hub
	src          StateSource
	log          *slog.Logger
	http         *http.Server
	snapInterval time.Duration
	assets       fs.FS // nil = the embedded UI; tests swap in an empty one
}

// NewServer builds the UI server.
func NewServer(addr string, hub *Hub, src StateSource, log *slog.Logger) *Server {
	return &Server{addr: addr, hub: hub, src: src, log: log, snapInterval: time.Second}
}

// runSnapshotLoop periodically publishes a fresh snapshot to the hub so connected
// dashboards update live (lights, mode, stream health, pairing state). Without
// this, an SSE client only ever sees the single snapshot built at connect time
// plus log events. The cadence is coarse on purpose — this is a status view, not
// a frame mirror.
func (s *Server) runSnapshotLoop(ctx context.Context) {
	interval := s.snapInterval
	if interval <= 0 {
		interval = time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// Skip when no browser is connected: BuildSnapshot reads the lights from
			// the Hue Bridge Pro (behind a cache), and the queue-sensitive Pro should not
			// be polled for an audience of nobody.
			if s.hub.hasSubscribers() {
				s.hub.SetSnapshot(BuildSnapshot(s.src))
			}
		}
	}
}

// Handler returns the routed HTTP handler behind kit's access log (routine requests
// at debug, probes never), baseline security headers (the UI loads nothing from
// elsewhere, so kit's default CSP fits) and panic recovery. AccessLog is outermost,
// so a recovered panic's 500 carries the request id; its writer passes Flush
// through, so the /api/events stream still streams.
func (s *Server) Handler() http.Handler {
	return s.wrap(s.routes())
}

// wrap is Handler's middleware around h. SecurityHeaders sits outside Recover:
// Recover restores the header it saw on entry before a panic's 500 (kit v0.13.0
// on), so inside it the 500 would lose them.
func (s *Server) wrap(h http.Handler) http.Handler {
	return httpx.Chain(h,
		httpx.AccessLog(s.log, httpx.AccessLogOptions{}),
		httpx.SecurityHeaders(""),
		httpx.Recover(s.log),
	)
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/state", s.handleState)
	mux.HandleFunc("GET /api/events", s.handleEvents)
	httpx.Health(mux, s.log)

	assets := s.assets
	if assets == nil {
		assets = web.FS()
	}
	if !spa.Built(assets) && s.log != nil {
		s.log.Warn("web UI not built into this binary; serving a placeholder (build with `make build`)")
	}
	// The UI has no client router and loads app.js/app.css by relative URL: only /
	// gets the shell, so a nested path is a 404 rather than a shell whose assets
	// resolve to more shells. /index.html stays a bookmarkable alias. An unbuilt binary
	// (a bare `go build` on a fresh clone) gets kit's "UI not built" page and a 503.
	mux.Handle("GET /", spa.Handler(assets, spa.Options{
		Routes: func(p string) bool { return p == "/" || p == "/index.html" },
	}))
	return mux
}

func (s *Server) handleState(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, BuildSnapshot(s.src))
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	st, err := sse.Open(w)
	if err != nil {
		if errors.Is(err, sse.ErrNoFlush) {
			httpx.WriteError(w, http.StatusInternalServerError, "streaming unsupported")
		}
		return
	}
	defer st.Close()

	// Initial paint: a fresh snapshot, then the buffered event tail. Subscribe AFTER the
	// tail replay: subscribing first would buffer events published during the replay
	// window onto the channel AND include them in the tail, delivering them twice (the
	// ring carries up to 200 events — too many to seed through the 32-deep channel). The
	// inverse gap (an event slipping through the window) is a pre-existing, cosmetic
	// live-log nicety, not worth a duplicate on every connect.
	snap := BuildSnapshot(s.src)
	if st.SendJSON("", Frame{Kind: "snapshot", Snapshot: &snap}) != nil {
		return
	}
	for _, e := range s.hub.Events() {
		if st.SendJSON("", Frame{Kind: "event", Event: &e}) != nil {
			return
		}
	}

	// ok is false only once the hub is closed for shutdown: the stream just ends.
	ch, cancel, ok := s.hub.Subscribe()
	if !ok {
		return
	}
	defer cancel()

	// A keepalive comment after 15s of silence keeps intermediaries from idle-closing
	// the stream AND surfaces a dead client: a write to a vanished peer eventually
	// errors and closes st.Done(), so the handler returns, cancel() runs, and the
	// snapshot loop stops polling the queue-sensitive Pro for an audience that is no
	// longer there.
	st.Heartbeat(15 * time.Second)
	for {
		select {
		case <-r.Context().Done():
			return
		case <-st.Done():
			return
		case f, ok := <-ch:
			// After shutdown cancels the request context, stop instead of draining
			// up to 32 queued frames to a client that may be stalled.
			if !ok || r.Context().Err() != nil {
				return
			}
			if st.SendJSON("", f) != nil {
				return
			}
		}
	}
}

// newHTTPServer builds the listening server with kit's canonical timeouts: a
// ReadHeaderTimeout (slow-loris bound) and no Read/WriteTimeout, which would
// truncate the long-lived /api/events SSE stream. It is split out so the timeouts
// are assertable.
//
// Shutdown closes the hub, ending every subscribed SSE stream, and cancels the
// context request contexts derive from, so a stream still in its initial paint or
// draining queued frames returns too instead of holding the drain until its timeout.
func (s *Server) newHTTPServer() *http.Server {
	srv := httpx.NewServer(s.addr, s.Handler())
	reqCtx, cancel := context.WithCancel(context.Background())
	srv.BaseContext = func(net.Listener) context.Context { return reqCtx }
	srv.RegisterOnShutdown(cancel)
	srv.RegisterOnShutdown(s.hub.Close)
	return srv
}

// Run serves until ctx is cancelled or the process is signalled. It returns a
// non-nil error only on a real bind/serve failure, so the caller can log it
// without taking down the headless service.
func (s *Server) Run(ctx context.Context) error {
	s.http = s.newHTTPServer()
	go s.runSnapshotLoop(ctx)
	return httpx.Serve(ctx, s.http, 3*time.Second)
}

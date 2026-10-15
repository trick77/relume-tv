package webui

import (
	"context"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/trick77/ship/kit/httpx"
	"github.com/trick77/ship/kit/spa"

	"github.com/trick77/relume-tv/web"
)

// notBuiltPage is kit spa's placeholder (503) when the binary was built without the
// UI copied into web/dist (a bare `go build` on a fresh clone): kit's default names
// another repo's make target.
const notBuiltPage = `<!doctype html><title>relume-tv</title><p>Web UI not built into this binary. Run <code>make build</code>.</p>`

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
// at debug, probes never), panic recovery and baseline security headers (the UI
// loads nothing from elsewhere, so kit's default CSP fits). AccessLog is outermost,
// so a recovered panic's 500 carries the request id; its writer passes Flush
// through, so the /api/events stream still streams.
func (s *Server) Handler() http.Handler {
	return httpx.Chain(s.routes(),
		httpx.AccessLog(s.log, httpx.AccessLogOptions{}),
		httpx.Recover(s.log),
		httpx.SecurityHeaders(""),
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
	mux.Handle("GET /", spa.Handler(assets, spa.Options{Placeholder: []byte(notBuiltPage)}))
	return mux
}

func (s *Server) handleState(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, BuildSnapshot(s.src))
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		httpx.WriteError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")

	write := func(b []byte) error {
		if _, err := w.Write(b); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}
	writeFrame := func(f Frame) error {
		b, _ := json.Marshal(f)
		out := append([]byte("data: "), b...)
		return write(append(out, '\n', '\n'))
	}

	// Initial paint: a fresh snapshot, then the buffered event tail. Subscribe AFTER the
	// tail replay: subscribing first would buffer events published during the replay
	// window onto the channel AND include them in the tail, delivering them twice (the
	// ring carries up to 200 events — too many to seed through the 32-deep channel). The
	// inverse gap (an event slipping through the window) is a pre-existing, cosmetic
	// live-log nicety, not worth a duplicate on every connect.
	snap := BuildSnapshot(s.src)
	if writeFrame(Frame{Kind: "snapshot", Snapshot: &snap}) != nil {
		return
	}
	for _, e := range s.hub.Events() {
		e := e
		if writeFrame(Frame{Kind: "event", Event: &e}) != nil {
			return
		}
	}

	ch, cancel := s.hub.Subscribe()
	defer cancel()

	// A periodic heartbeat (an SSE comment) keeps intermediaries from idle-closing the
	// stream AND surfaces a dead client: a write to a vanished peer eventually errors, so
	// the handler returns, cancel() runs, and the snapshot loop stops polling the
	// queue-sensitive Pro for an audience that is no longer there.
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			if write([]byte(": ping\n\n")) != nil {
				return
			}
		case f, ok := <-ch:
			if !ok {
				return
			}
			if writeFrame(f) != nil {
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
// Request contexts derive from a context cancelled when shutdown begins, so open
// SSE streams return at once instead of holding the drain until its timeout.
func (s *Server) newHTTPServer() *http.Server {
	srv := httpx.NewServer(s.addr, s.Handler())
	reqCtx, cancel := context.WithCancel(context.Background())
	srv.BaseContext = func(net.Listener) context.Context { return reqCtx }
	srv.RegisterOnShutdown(cancel)
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

package entertainment

import (
	"context"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/dtls/v3"
	"github.com/trick77/relume-tv/internal/huestream"
)

// TestReceiver_decodesStreamOverDTLS drives the receiver end-to-end: a pion DTLS
// client authenticates with the PSK and streams a HueStream v1 frame, and the
// receiver decrypts + decodes it (observed via OnFrame).
func TestReceiver_decodesStreamOverDTLS(t *testing.T) {
	const identity = "tvuser"
	psk := []byte{0xDE, 0xAD, 0xBE, 0xEF, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0A, 0x0B, 0x0C}
	const port = 32100

	frames := make(chan *huestream.Frame, 4)
	r := &Receiver{
		bindIP: "127.0.0.1",
		Port:   port,
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		lookup: func(id string) ([]byte, bool) {
			if id == identity {
				return psk, true
			}
			return nil, false
		},
		OnFrame: func(_ string, f *huestream.Frame) { frames <- f },
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = r.Run(ctx) }()

	// Wait for the listener to come up, then connect a DTLS-PSK client.
	var conn *dtls.Conn
	deadline := time.Now().Add(5 * time.Second)
	for {
		// SA1019: pion/dtls v3 deprecates Dial and Config in favour of the
		// options API. Migrating the test client is a behaviour change to the
		// DTLS handshake path and belongs in its own change, not a lint pass.
		//nolint:staticcheck // SA1019: deliberate, see above
		c, err := dtls.Dial("udp",
			&net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: port},
			&dtls.Config{ //nolint:staticcheck // SA1019
				PSK:                  func([]byte) ([]byte, error) { return psk, nil },
				PSKIdentityHint:      []byte(identity),
				CipherSuites:         []dtls.CipherSuiteID{dtls.TLS_PSK_WITH_AES_128_GCM_SHA256},
				ExtendedMasterSecret: dtls.DisableExtendedMasterSecret,
			})
		if err == nil {
			conn = c
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("dtls dial: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	defer conn.Close()

	if _, err := conn.Write(v1FrameLightsSixEleven()); err != nil {
		t.Fatalf("write frame: %v", err)
	}

	select {
	case f := <-frames:
		if len(f.Channels) != 2 || f.Channels[0].ID != 6 || f.Channels[1].ID != 11 {
			t.Fatalf("decoded channels = %+v", f.Channels)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a decoded frame")
	}
}

// TestReceiver_slowSinkDropsFramesWithoutBlocking verifies the bounded queue (L2):
// a slow OnFrame must not stall the reader — excess frames are dropped, not blocked,
// so the reader keeps draining the socket. With a sink gated shut, far fewer frames
// are delivered than were sent, and the reader still completes.
func TestReceiver_slowSinkDropsFramesWithoutBlocking(t *testing.T) {
	gate := make(chan struct{})
	var delivered int32
	r := &Receiver{
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		OnFrame: func(_ string, _ *huestream.Frame) {
			<-gate // sink is blocked until released
			atomic.AddInt32(&delivered, 1)
		},
	}

	client, server := net.Pipe()
	handleDone := make(chan struct{})
	go func() {
		r.handle(context.Background(), server)
		close(handleDone)
	}()

	const sent = 40
	frame := v1FrameLightsSixEleven()
	for i := 0; i < sent; i++ {
		if _, err := client.Write(frame); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	client.Close() // reader sees EOF, closes the queue

	close(gate) // release the sink; forwarder drains whatever is still queued
	select {
	case <-handleDone:
	case <-time.After(5 * time.Second):
		t.Fatal("handle did not return")
	}

	got := atomic.LoadInt32(&delivered)
	if got >= sent {
		t.Fatalf("delivered %d of %d — expected the slow sink to drop frames", got, sent)
	}
	if got == 0 {
		t.Fatalf("delivered 0 frames — the queue should still pass some through")
	}
}

// eventLog records OnStreamStart/OnStreamStop calls in order.
type eventLog struct {
	mu     sync.Mutex
	events []string
	ch     chan string
}

func newEventLog() *eventLog { return &eventLog{ch: make(chan string, 16)} }

func (l *eventLog) add(e string) {
	l.mu.Lock()
	l.events = append(l.events, e)
	l.mu.Unlock()
	l.ch <- e
}

func (l *eventLog) wait(t *testing.T, want string) {
	t.Helper()
	select {
	case got := <-l.ch:
		if got != want {
			t.Fatalf("event = %q, want %q (all so far: %v)", got, want, l.all())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for event %q (all so far: %v)", want, l.all())
	}
}

func (l *eventLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

func TestReceiver_idleSessionClosesAndFiresStopOnce(t *testing.T) {
	// Given: a receiver with a short idle timeout and a TV that goes silent
	events := newEventLog()
	r := &Receiver{
		log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		IdleTimeout:   50 * time.Millisecond,
		OnStreamStart: func(string) { events.add("start") },
		OnStreamStop:  func(string) { events.add("stop") },
	}
	client, server := net.Pipe()
	defer client.Close()
	handleDone := make(chan struct{})
	go func() { r.handle(context.Background(), server); close(handleDone) }()
	events.wait(t, "start")
	if _, err := client.Write(v1FrameLightsSixEleven()); err != nil {
		t.Fatalf("write: %v", err)
	}

	// When: no further frames arrive (the client never closes)

	// Then: the session ends on its own and OnStreamStop fires exactly once
	events.wait(t, "stop")
	select {
	case <-handleDone:
	case <-time.After(5 * time.Second):
		t.Fatal("handle did not return after the idle timeout")
	}
	if got := events.all(); len(got) != 2 {
		t.Fatalf("events = %v; want exactly [start stop]", got)
	}
}

func TestReceiver_newSessionStopsThePreviousOneBeforeStarting(t *testing.T) {
	// Given: a live TV session that never sends a close_notify
	events := newEventLog()
	r := &Receiver{
		log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		IdleTimeout:   time.Minute, // idle must not be what ends the first session
		OnStreamStart: func(remote string) { events.add("start " + remote) },
		OnStreamStop:  func(remote string) { events.add("stop " + remote) },
	}
	clientA, serverA := net.Pipe()
	defer clientA.Close()
	go r.handle(context.Background(), serverA)
	events.wait(t, "start "+serverA.RemoteAddr().String())

	// When: a second handshake completes while the first is still open
	clientB, serverB := net.Pipe()
	doneB := make(chan struct{})
	go func() { r.handle(context.Background(), serverB); close(doneB) }()

	// Then: the first session is stopped before the second starts (never
	// interleaved, so the streamer's Stop cannot tear down the new Pro path)
	events.wait(t, "stop "+serverA.RemoteAddr().String())
	events.wait(t, "start "+serverB.RemoteAddr().String())

	clientB.Close()
	events.wait(t, "stop "+serverB.RemoteAddr().String())
	<-doneB
}

// v1FrameLightsSixEleven builds a minimal HueStream v1 RGB frame for lights 6/11.
func v1FrameLightsSixEleven() []byte {
	b := []byte("HueStream")
	b = append(b, 0x01, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00)
	b = append(b, 0x00, 0x00, 0x06, 0xFF, 0xFF, 0x00, 0x00, 0x00, 0x00)
	b = append(b, 0x00, 0x00, 0x0B, 0x00, 0x00, 0xFF, 0xFF, 0x00, 0x00)
	return b
}

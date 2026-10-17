package webui

import "testing"

func TestHub_RingBufferDropsOldest(t *testing.T) {
	h := NewHub(2)
	h.PublishEvent(Event{Msg: "a"})
	h.PublishEvent(Event{Msg: "b"})
	h.PublishEvent(Event{Msg: "c"})
	got := h.Events()
	if len(got) != 2 || got[0].Msg != "b" || got[1].Msg != "c" {
		t.Fatalf("ring buffer = %+v, want [b c]", got)
	}
}

func TestHub_SubscribeReceivesEventAndSnapshot(t *testing.T) {
	h := NewHub(8)
	ch, cancel, ok := h.Subscribe()
	if !ok {
		t.Fatal("Subscribe refused on an open hub")
	}
	defer cancel()

	h.PublishEvent(Event{Time: "t", Level: "INFO", Msg: "hi"})
	f := <-ch
	if f.Kind != "event" || f.Event == nil || f.Event.Msg != "hi" {
		t.Fatalf("event frame = %+v", f)
	}

	h.SetSnapshot(Snapshot{Version: "test"})
	f = <-ch
	if f.Kind != "snapshot" || f.Snapshot == nil || f.Snapshot.Version != "test" {
		t.Fatalf("snapshot frame = %+v", f)
	}
}

func TestHub_SnapshotEvictsWhenFull(t *testing.T) {
	// Given: a stalled subscriber whose buffer is full of events
	h := NewHub(8)
	ch, cancel, _ := h.Subscribe()
	defer cancel()
	for range subBuffer + 8 {
		h.PublishEvent(Event{Msg: "flood"})
	}

	// When
	h.SetSnapshot(Snapshot{Version: "fresh"})

	// Then: the fresh snapshot still landed (an event would have been dropped)
	for {
		select {
		case f := <-ch:
			if f.Kind == "snapshot" && f.Snapshot.Version == "fresh" {
				return
			}
		default:
			t.Fatal("snapshot dropped on a full subscriber; dashboard stays stale")
		}
	}
}

func TestHub_HasSubscribers(t *testing.T) {
	h := NewHub(8)
	if h.hasSubscribers() {
		t.Fatal("fresh hub should have no subscribers")
	}
	_, cancel, _ := h.Subscribe()
	if !h.hasSubscribers() {
		t.Fatal("expected a subscriber after Subscribe")
	}
	cancel()
	if h.hasSubscribers() {
		t.Fatal("expected no subscribers after cancel")
	}
}

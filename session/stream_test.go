package session

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/ionalpha/flynn/bus"
	"github.com/ionalpha/flynn/spine"
)

func newTestStream(t *testing.T) *Stream {
	t.Helper()
	return newStream(spine.NewMemoryLog(), bus.NewMemory(), "test")
}

// collect reads n events from ch or fails after a generous timeout.
func collect(t *testing.T, ch <-chan Event, n int) []Event {
	t.Helper()
	out := make([]Event, 0, n)
	deadline := time.After(2 * time.Second)
	for len(out) < n {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("stream closed after %d/%d events", len(out), n)
			}
			out = append(out, ev)
		case <-deadline:
			t.Fatalf("timed out after %d/%d events", len(out), n)
		}
	}
	return out
}

func assertSeqs(t *testing.T, evs []Event, want ...int64) {
	t.Helper()
	if len(evs) != len(want) {
		t.Fatalf("got %d events, want %d", len(evs), len(want))
	}
	for i, e := range evs {
		if e.Seq != want[i] {
			t.Fatalf("event %d seq = %d, want %d", i, e.Seq, want[i])
		}
	}
}

// TestStreamCatchUp: events appended before a subscription are replayed in order.
func TestStreamCatchUp(t *testing.T) {
	s := newTestStream(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := s.append(ctx, Event{Kind: KindTurnStarted, Turn: i + 1}); err != nil {
			t.Fatal(err)
		}
	}

	subCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	ch, err := s.Subscribe(subCtx, 0)
	if err != nil {
		t.Fatal(err)
	}
	assertSeqs(t, collect(t, ch, 3), 1, 2, 3)
}

// TestStreamTail: events appended after a subscription arrive live.
func TestStreamTail(t *testing.T) {
	s := newTestStream(t)
	subCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := s.Subscribe(subCtx, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := s.append(context.Background(), Event{Kind: KindTurnStarted, Turn: i + 1}); err != nil {
			t.Fatal(err)
		}
	}
	assertSeqs(t, collect(t, ch, 3), 1, 2, 3)
}

// TestStreamCatchUpThenTail: a subscriber that joins mid-stream gets the backlog
// then the live tail as one continuous, ordered sequence.
func TestStreamCatchUpThenTail(t *testing.T) {
	s := newTestStream(t)
	ctx := context.Background()
	_ = s.append(ctx, Event{Kind: KindSessionStarted})
	_ = s.append(ctx, Event{Kind: KindTurnStarted, Turn: 1})

	subCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	ch, err := s.Subscribe(subCtx, 0)
	if err != nil {
		t.Fatal(err)
	}
	// One from the backlog read, then keep appending while subscribed.
	first := collect(t, ch, 2)
	assertSeqs(t, first, 1, 2)
	_ = s.append(ctx, Event{Kind: KindTurnCompleted, Turn: 1})
	_ = s.append(ctx, Event{Kind: KindConverged})
	assertSeqs(t, collect(t, ch, 2), 3, 4)
}

// TestStreamAfterSeq: subscribing with an offset skips everything up to and
// including it.
func TestStreamAfterSeq(t *testing.T) {
	s := newTestStream(t)
	ctx := context.Background()
	for i := 0; i < 4; i++ {
		_ = s.append(ctx, Event{Kind: KindTurnStarted, Turn: i + 1})
	}
	subCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	ch, err := s.Subscribe(subCtx, 2)
	if err != nil {
		t.Fatal(err)
	}
	assertSeqs(t, collect(t, ch, 2), 3, 4)
}

// TestStreamFanOut: every subscriber independently sees the full stream.
func TestStreamFanOut(t *testing.T) {
	s := newTestStream(t)
	subCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a, err := s.Subscribe(subCtx, 0)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Subscribe(subCtx, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		_ = s.append(context.Background(), Event{Kind: KindTurnStarted, Turn: i + 1})
	}
	assertSeqs(t, collect(t, a, 5), 1, 2, 3, 4, 5)
	assertSeqs(t, collect(t, b, 5), 1, 2, 3, 4, 5)
}

// TestStreamBurstNoLoss: a burst of appends coalesces many bus wakes into far
// fewer drains, yet the subscriber still receives every event in order. This is
// the property that makes the at-most-once bus safe as a mere liveness signal.
func TestStreamBurstNoLoss(t *testing.T) {
	s := newTestStream(t)
	subCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := s.Subscribe(subCtx, 0)
	if err != nil {
		t.Fatal(err)
	}
	const n = 200
	go func() {
		for i := 0; i < n; i++ {
			_ = s.append(context.Background(), Event{Kind: KindTurnStarted, Turn: i + 1})
		}
	}()
	evs := collect(t, ch, n)
	for i, e := range evs {
		if e.Seq != int64(i+1) {
			t.Fatalf("event %d seq = %d, want %d", i, e.Seq, i+1)
		}
	}
}

// TestStreamPayloadRoundTrip: the typed event body survives the spine codec, so a
// subscriber reconstructs tool calls and results exactly.
func TestStreamPayloadRoundTrip(t *testing.T) {
	s := newTestStream(t)
	ctx := context.Background()
	in := Event{
		Kind:      KindToolCall,
		Turn:      2,
		Tool:      "write",
		ToolUseID: "c7",
		Input:     json.RawMessage(`{"path":"x.txt","content":"hi"}`),
	}
	if err := s.append(ctx, in); err != nil {
		t.Fatal(err)
	}
	subCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	ch, err := s.Subscribe(subCtx, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := collect(t, ch, 1)[0]
	if got.Kind != KindToolCall || got.Tool != "write" || got.ToolUseID != "c7" ||
		got.Turn != 2 || string(got.Input) != string(in.Input) {
		t.Fatalf("round-tripped event = %+v", got)
	}
	if got.Seq != 1 || got.Time.IsZero() {
		t.Fatalf("log-assigned fields missing: seq=%d time=%v", got.Seq, got.Time)
	}
}

// TestStreamCancelClosesChannel: cancelling the subscription context ends delivery
// and closes the channel.
func TestStreamCancelClosesChannel(t *testing.T) {
	s := newTestStream(t)
	subCtx, cancel := context.WithCancel(context.Background())
	ch, err := s.Subscribe(subCtx, 0)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	// The channel must close; any in-flight event already buffered is drained first.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return // closed: the subscription tore down cleanly
			}
		case <-deadline:
			t.Fatal("channel not closed after context cancel")
		}
	}
}

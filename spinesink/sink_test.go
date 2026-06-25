package spinesink_test

import (
	"context"
	"testing"
	"time"

	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/spine"
	"github.com/ionalpha/flynn/spinesink"
)

// TestSinkRecordsDispatchOntoSpine wires the dispatch waist to the spine and
// asserts an action's lifecycle lands on the run's stream end to end.
func TestSinkRecordsDispatchOntoSpine(t *testing.T) {
	ctx := context.Background()
	at := time.Unix(9000, 0)
	clk := clock.NewManual(at)
	log := spine.NewMemoryLog(spine.WithClock(clk))

	d := dispatch.New(
		dispatch.WithClock(clk),
		dispatch.WithEventSink(spinesink.New(log, "run-1")),
	)

	work := func(context.Context) (dispatch.Metering, error) { return dispatch.Metering{Tokens: 3}, nil }
	if err := d.Govern(ctx, dispatch.Action{Name: "search"}, work); err != nil {
		t.Fatalf("govern: %v", err)
	}

	events, err := log.Read(ctx, spine.Query{Stream: "run-1"})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("got %d events, want start+end", len(events))
	}
	if events[0].Type != dispatch.EventStart || events[1].Type != dispatch.EventEnd {
		t.Fatalf("event types = %q,%q", events[0].Type, events[1].Type)
	}
	if events[0].Actor != spine.ActorAgent {
		t.Fatalf("actor = %q, want agent", events[0].Actor)
	}
	if got := events[0].Payload["action"]; got != "search" {
		t.Fatalf("payload action = %v, want search", got)
	}
	// The dispatcher's clock time is preserved on the spine.
	if !events[0].Time.Equal(at.UTC()) {
		t.Fatalf("event Time = %v, want %v", events[0].Time, at.UTC())
	}
}

// TestSinkStampsPrincipal asserts the governed events carry the principal bound to
// the run's context, the audit "who", and leave it empty when none is bound.
func TestSinkStampsPrincipal(t *testing.T) {
	run := func(ctx context.Context) spine.Event {
		t.Helper()
		log := spine.NewMemoryLog()
		d := dispatch.New(dispatch.WithEventSink(spinesink.New(log, "run-1")))
		work := func(context.Context) (dispatch.Metering, error) { return dispatch.Metering{}, nil }
		if err := d.Govern(ctx, dispatch.Action{Name: "search"}, work); err != nil {
			t.Fatalf("govern: %v", err)
		}
		events, err := log.Read(ctx, spine.Query{Stream: "run-1"})
		if err != nil || len(events) == 0 {
			t.Fatalf("read: %v (%d events)", err, len(events))
		}
		return events[0]
	}

	if got := run(capability.WithPrincipal(context.Background(), "agent-7")).Principal; got != "agent-7" {
		t.Fatalf("bound principal not stamped: %q", got)
	}
	if got := run(context.Background()).Principal; got != "" {
		t.Fatalf("unbound principal must be empty, got %q", got)
	}
}

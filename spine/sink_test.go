package spine_test

import (
	"context"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/spine"
)

// TestSinkRecordsDispatchOntoSpine wires the dispatch waist to the spine and
// asserts an action's lifecycle lands on the run's stream end to end.
func TestSinkRecordsDispatchOntoSpine(t *testing.T) {
	ctx := context.Background()
	at := time.Unix(9000, 0)
	clk := clock.NewManual(at)
	log := spine.NewMemoryLog(spine.WithClock(clk))

	d := dispatch.New(
		dispatch.HandlerFunc(func(context.Context, dispatch.Action) (dispatch.Result, error) {
			return dispatch.Result{Tokens: 3}, nil
		}),
		dispatch.WithClock(clk),
		dispatch.WithEventSink(spine.NewSink(log, "run-1")),
	)

	if _, err := d.Dispatch(ctx, dispatch.Action{Name: "search"}); err != nil {
		t.Fatalf("dispatch: %v", err)
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

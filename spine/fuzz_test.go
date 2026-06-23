package spine_test

import (
	"context"
	"testing"

	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/spine"
	"github.com/ionalpha/flynn/state"
)

// FuzzSinkPayload throws arbitrary action names, scopes, and error classes at
// the dispatch->spine sink and asserts the mapping never panics, always lands
// exactly one event, and preserves the action — the payload must survive any
// string (it is JSON-serialised by the durable log).
func FuzzSinkPayload(f *testing.F) {
	f.Add("fetch", "alpha", "")
	f.Add("", "", "transient")
	f.Add("%s\x00", "naïve\n", "boom")

	f.Fuzz(func(t *testing.T, action, project, errClass string) {
		ctx := context.Background()
		log := spine.NewMemoryLog()
		sink := spine.NewSink(log, "run")

		ev := dispatch.Event{
			Type:   dispatch.EventEnd,
			Action: action,
			Scope:  state.Scope{Project: project},
			Err:    errClass,
		}
		if err := sink.Append(ctx, ev); err != nil {
			t.Fatalf("append: %v", err)
		}

		got, err := log.Read(ctx, spine.Query{Stream: "run"})
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("got %d events, want 1", len(got))
		}
		if got[0].Payload["action"] != action {
			t.Fatalf("action not preserved: %v != %q", got[0].Payload["action"], action)
		}
	})
}

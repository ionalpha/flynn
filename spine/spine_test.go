package spine_test

import (
	"context"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/spine"
)

// Per-stream monotonic Seq, read paging, and payload immutability are covered
// for every backend by spinetest.RunSuite (see conformance_test.go). The tests
// below are MemoryLog-specific: clock injection, and the Fold projection helper.

func TestMemoryLogStampsFromClock(t *testing.T) {
	ctx := context.Background()
	at := time.Unix(4242, 0).UTC()
	log := spine.NewMemoryLog(spine.WithClock(clock.NewManual(at)))

	e, err := log.Append(ctx, spine.AppendInput{Stream: "s", Type: "e", Actor: spine.ActorAgent})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if !e.Time.Equal(at) {
		t.Fatalf("event Time = %v, want the clock's %v", e.Time, at)
	}
}

func TestFoldProjectsState(t *testing.T) {
	ctx := context.Background()
	log := spine.NewMemoryLog()
	for _, typ := range []string{"start", "tool", "tool", "end"} {
		if _, err := log.Append(ctx, spine.AppendInput{Stream: "s", Type: typ, Actor: spine.ActorAgent}); err != nil {
			t.Fatal(err)
		}
	}
	events, err := log.Read(ctx, spine.Query{Stream: "s"})
	if err != nil {
		t.Fatal(err)
	}

	// State is a projection of the log: count tool events.
	tools := spine.Fold(events, 0, func(n int, e spine.Event) int {
		if e.Type == "tool" {
			return n + 1
		}
		return n
	})
	if tools != 2 {
		t.Fatalf("projected tool count = %d, want 2", tools)
	}
}

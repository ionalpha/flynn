package spine_test

import (
	"context"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/spine"
)

func TestMemoryLogPerStreamMonotonicSeq(t *testing.T) {
	ctx := context.Background()
	log := spine.NewMemoryLog()

	for i := 0; i < 3; i++ {
		e, err := log.Append(ctx, spine.AppendInput{Stream: "run-a", Type: "tick", Actor: spine.ActorAgent})
		if err != nil {
			t.Fatalf("append a: %v", err)
		}
		if want := int64(i + 1); e.Seq != want {
			t.Fatalf("run-a event %d Seq = %d, want %d", i, e.Seq, want)
		}
	}
	// A second stream has its own independent sequence.
	e, err := log.Append(ctx, spine.AppendInput{Stream: "run-b", Type: "tick", Actor: spine.ActorAgent})
	if err != nil {
		t.Fatalf("append b: %v", err)
	}
	if e.Seq != 1 {
		t.Fatalf("run-b first Seq = %d, want 1 (streams are independent)", e.Seq)
	}
}

func TestMemoryLogReadQuery(t *testing.T) {
	ctx := context.Background()
	log := spine.NewMemoryLog()
	for i := 0; i < 5; i++ {
		if _, err := log.Append(ctx, spine.AppendInput{Stream: "s", Type: "e", Actor: spine.ActorSystem}); err != nil {
			t.Fatal(err)
		}
	}

	all, err := log.Read(ctx, spine.Query{Stream: "s"})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(all) != 5 {
		t.Fatalf("read all = %d, want 5", len(all))
	}

	// AfterSeq is an exclusive lower bound; Limit caps the result.
	page, err := log.Read(ctx, spine.Query{Stream: "s", AfterSeq: 2, Limit: 2})
	if err != nil {
		t.Fatalf("read page: %v", err)
	}
	if len(page) != 2 || page[0].Seq != 3 || page[1].Seq != 4 {
		t.Fatalf("page = %+v, want Seq 3,4", page)
	}
}

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

func TestAppendDecouplesPayloadFromCaller(t *testing.T) {
	ctx := context.Background()
	log := spine.NewMemoryLog()

	in := map[string]any{"k": "v"}
	if _, err := log.Append(ctx, spine.AppendInput{Stream: "s", Type: "e", Actor: spine.ActorAgent, Payload: in}); err != nil {
		t.Fatal(err)
	}
	in["k"] = "mutated" // a caller mutating its map after Append must not change history

	events, err := log.Read(ctx, spine.Query{Stream: "s"})
	if err != nil {
		t.Fatal(err)
	}
	if got := events[0].Payload["k"]; got != "v" {
		t.Fatalf("stored payload = %v, want v (the log must be immutable)", got)
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

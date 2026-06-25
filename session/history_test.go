package session

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ionalpha/flynn/spine"
)

// TestHistory checks that History reads back exactly the events recorded for a run,
// in order and losslessly, which is what flynn inspect and replay depend on.
func TestHistory(t *testing.T) {
	ctx := context.Background()
	log := spine.NewMemoryLog()
	const id = "run-xyz"

	in := []Event{
		{Kind: KindSessionStarted, Actor: spine.ActorSystem, Text: "do the thing"},
		{Kind: KindToolCall, Actor: spine.ActorAgent, Tool: "bash", ToolUseID: "t1", Input: json.RawMessage(`{"command":"ls"}`)},
		{Kind: KindToolResult, Actor: spine.ActorAgent, Tool: "bash", ToolUseID: "t1", Result: "a\nb"},
		{Kind: KindConverged, Actor: spine.ActorSystem, Text: "done"},
	}
	for _, e := range in {
		if _, err := log.Append(ctx, e.toAppend(id)); err != nil {
			t.Fatal(err)
		}
	}

	got, err := History(ctx, log, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(in) {
		t.Fatalf("got %d events, want %d", len(got), len(in))
	}
	if got[0].Kind != KindSessionStarted || got[0].Text != "do the thing" {
		t.Fatalf("started event not preserved: %+v", got[0])
	}
	if got[1].Kind != KindToolCall || got[1].Tool != "bash" || string(got[1].Input) != `{"command":"ls"}` {
		t.Fatalf("tool call not preserved: %+v", got[1])
	}
	if got[2].Kind != KindToolResult || got[2].Result != "a\nb" {
		t.Fatalf("tool result not preserved: %+v", got[2])
	}
	if got[3].Kind != KindConverged || got[3].Text != "done" {
		t.Fatalf("converged event not preserved: %+v", got[3])
	}

	// Seq is monotonic in append order.
	for i := 1; i < len(got); i++ {
		if got[i].Seq <= got[i-1].Seq {
			t.Fatalf("seq not increasing at %d: %d <= %d", i, got[i].Seq, got[i-1].Seq)
		}
	}

	// An unknown run id is an empty history, not an error.
	empty, err := History(ctx, log, "no-such-run")
	if err != nil || len(empty) != 0 {
		t.Fatalf("unknown id: err=%v len=%d, want nil/0", err, len(empty))
	}
}

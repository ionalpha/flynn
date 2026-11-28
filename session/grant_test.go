package session

import (
	"context"
	"testing"
	"time"

	"github.com/ionalpha/flynn/bus"
	"github.com/ionalpha/flynn/spine"
)

// TestRecordGrantSurvivesCancellation replays what was recorded under a context
// that had already ended, as it has when the user stopped waiting.
func TestRecordGrantSurvivesCancellation(t *testing.T) {
	log := spine.NewMemoryLog()
	sess := New(log, bus.NewMemory())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	want := SandboxGrant{Dir: `C:\repo`, State: GrantCancelled, ElapsedMS: 104000}
	sess.RecordGrant(ctx, want, "")
	sess.RecordGrant(ctx, SandboxGrant{Dir: `C:\repo`, State: GrantFailed}, "refused")

	evs, err := History(context.Background(), log, sess.ID())
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 {
		t.Fatalf("replayed %d events, want 2: %+v", len(evs), evs)
	}
	first := evs[0]
	if first.Kind != KindSandboxGrant || first.SandboxGrant == nil || *first.SandboxGrant != want {
		t.Fatalf("first record = %+v", first)
	}
	if got := first.SandboxGrant.Elapsed(); got != 104*time.Second {
		t.Fatalf("elapsed = %s", got)
	}
	if evs[1].Err != "refused" {
		t.Fatalf("a failed grant's reason = %q", evs[1].Err)
	}
}

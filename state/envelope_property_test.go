package state_test

import (
	"context"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/hlc"
	"github.com/ionalpha/flynn/internal/testkit"
	"github.com/ionalpha/flynn/state"
)

// TestProp_WritesAdvanceEnvelope: for any generated sequence of skill writes,
// the sync envelope stays consistent — UpdatedHLC strictly advances and the
// last-writer is stamped — whether each write creates or updates.
func TestProp_WritesAdvanceEnvelope(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		ctx := context.Background()
		p := state.NewMemory(state.WithInstanceID("n1"))

		var last hlc.Time
		for n := rapid.IntRange(1, 25).Draw(rt, "n"); n > 0; n-- {
			sk := testkit.SkillGen().Draw(rt, "skill")
			saved, err := p.Skills().Upsert(ctx, sk)
			if err != nil {
				rt.Fatalf("upsert: %v", err)
			}
			if !last.Before(saved.UpdatedHLC) {
				rt.Fatalf("UpdatedHLC did not advance: %v then %v", last, saved.UpdatedHLC)
			}
			if saved.LastWriterID != "n1" {
				rt.Fatalf("LastWriterID = %q, want n1", saved.LastWriterID)
			}
			last = saved.UpdatedHLC
		}
	})
}

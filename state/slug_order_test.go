package state_test

import (
	"context"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/state"
)

// A slug held in two scopes must resolve to the same skill on every lookup: the
// oldest one, which is what the SQL backends return.
func TestSlugSharedAcrossScopesResolvesToTheOldest(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewManual(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	skills := state.NewMemory(state.WithClock(clk)).Skills()

	var oldest string
	for i, project := range []string{"a", "b", "c", "d", "e", "f"} {
		sk, err := skills.Upsert(ctx, state.Skill{Slug: "shared", Name: "shared", Scope: state.Scope{Project: project}})
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			oldest = sk.ID
		}
		clk.Advance(time.Second)
	}
	for range 50 {
		got, err := skills.Get(ctx, "shared")
		if err != nil {
			t.Fatal(err)
		}
		if got.ID != oldest {
			t.Fatalf("Get by slug returned %s, want the oldest, %s", got.ID, oldest)
		}
	}
}

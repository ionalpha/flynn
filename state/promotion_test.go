package state_test

import (
	"context"
	"testing"

	"github.com/ionalpha/flynn/spine"
	"github.com/ionalpha/flynn/state"
)

func TestPromotionDecisionValid(t *testing.T) {
	cases := []struct {
		name string
		d    state.PromotionDecision
		want bool
	}{
		{"item and reviewer", state.PromotionDecision{MemoryID: "m", By: "user:operator"}, true},
		{"a revocation is just as valid", state.PromotionDecision{MemoryID: "m", By: "curator"}, true},
		{"no reviewer", state.PromotionDecision{MemoryID: "m"}, false},
		{"no item", state.PromotionDecision{By: "user:operator"}, false},
		{"nothing at all", state.PromotionDecision{}, false},
	}
	for _, c := range cases {
		if got := c.d.Valid(); got != c.want {
			t.Errorf("%s: Valid = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestPromotedSet(t *testing.T) {
	rows := []state.MemoryPromotion{
		{MemoryID: "a", Promoted: true, By: "user:operator"},
		{MemoryID: "b", By: "user:operator", Reason: "revoked"},
		{MemoryID: "c", Promoted: true, By: "curator"},
	}
	set := state.PromotedSet(rows)
	if len(set) != 2 || !set["a"] || !set["c"] {
		t.Fatalf("PromotedSet = %v, want just a and c", set)
	}
	// A revoked row is a decision and not a promotion, so it must not read as one.
	if set["b"] {
		t.Fatal("a revoked row is in the promoted set")
	}
	if got := state.PromotedSet(nil); len(got) != 0 {
		t.Fatalf("PromotedSet(nil) = %v, want empty", got)
	}
}

func TestSortPromotions(t *testing.T) {
	rows := []state.MemoryPromotion{{MemoryID: "c"}, {MemoryID: "a"}, {MemoryID: "b"}}
	state.SortPromotions(rows)
	for i, want := range []string{"a", "b", "c"} {
		if rows[i].MemoryID != want {
			t.Fatalf("sorted = %+v, want ordered by item", rows)
		}
	}
}

// TestPromotionsSurviveReplayAndSnapshot pins a promotion as ordinary event-sourced
// state. A snapshot that dropped the decisions would look correct until the stream
// got long enough to checkpoint, and then quietly un-promote everything: the digest
// would shrink to the operator's own memory with nothing anywhere reporting why.
func TestPromotionsSurviveReplayAndSnapshot(t *testing.T) {
	ctx := context.Background()
	for _, snapshotEvery := range []int{0, 1} {
		log := spine.NewMemoryLog()
		opts := []state.Option{state.WithEventLog(log)}
		if snapshotEvery > 0 {
			opts = append(opts, state.WithSnapshotEvery(snapshotEvery))
		}
		mem := state.NewMemory(opts...).Memory()

		a, err := mem.Write(ctx, state.MemoryItem{Kind: "fact", Content: "one"})
		if err != nil {
			t.Fatal(err)
		}
		b, err := mem.Write(ctx, state.MemoryItem{Kind: "fact", Content: "two"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := mem.Promote(ctx, state.PromotionDecision{MemoryID: a.ID, Promoted: true, By: "user:operator"}); err != nil {
			t.Fatal(err)
		}
		// A revision, so the fold has to reproduce the decision in force rather than
		// the first one it saw.
		if _, err := mem.Promote(ctx, state.PromotionDecision{MemoryID: b.ID, Promoted: true, By: "curator"}); err != nil {
			t.Fatal(err)
		}
		if _, err := mem.Promote(ctx, state.PromotionDecision{MemoryID: b.ID, By: "user:operator", Reason: "stale"}); err != nil {
			t.Fatal(err)
		}
		want, err := mem.Promotions(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}

		replayed, err := state.Replay(ctx, log)
		if err != nil {
			t.Fatal(err)
		}
		got, err := replayed.Memory().Promotions(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(want) {
			t.Fatalf("snapshotEvery=%d: replayed %d promotion rows, want %d", snapshotEvery, len(got), len(want))
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("snapshotEvery=%d: replayed promotion %+v, want %+v", snapshotEvery, got[i], want[i])
			}
		}
	}
}

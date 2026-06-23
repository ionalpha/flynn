package state_test

import (
	"context"
	"errors"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/internal/testkit"
	"github.com/ionalpha/flynn/state"
)

// TestWithInstanceID is memory-provider-specific: the constructor option sets
// the origin/last-writer instance stamped onto records.
func TestWithInstanceID(t *testing.T) {
	ctx := context.Background()
	p := state.NewMemory(state.WithInstanceID("node-7"))
	sk, err := p.Skills().Upsert(ctx, state.Skill{Slug: "k"})
	if err != nil {
		t.Fatal(err)
	}
	if sk.OriginInstanceID != "node-7" || sk.LastWriterID != "node-7" {
		t.Fatalf("origin/writer = %q/%q, want node-7", sk.OriginInstanceID, sk.LastWriterID)
	}
}

// TestProp_DeletedSkillNeverSurfaces: once deleted, a skill built from any
// generated input never appears in Get, List, or Search.
func TestProp_DeletedSkillNeverSurfaces(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		ctx := context.Background()
		p := state.NewMemory()
		saved, err := p.Skills().Upsert(ctx, testkit.SkillGen().Draw(rt, "skill"))
		if err != nil {
			rt.Fatal(err)
		}
		if err := p.Skills().Delete(ctx, saved.ID); err != nil {
			rt.Fatal(err)
		}
		if _, err := p.Skills().Get(ctx, saved.ID); !errors.Is(err, state.ErrNotFound) {
			rt.Fatalf("Get surfaced a tombstone: %v", err)
		}
		if got, _ := p.Skills().List(ctx, saved.Scope); len(got) != 0 {
			rt.Fatalf("List surfaced %d tombstones", len(got))
		}
		hits, _ := p.Skills().Search(ctx, "", 0)
		for _, h := range hits {
			if h.ID == saved.ID {
				rt.Fatal("Search surfaced a tombstone")
			}
		}
	})
}

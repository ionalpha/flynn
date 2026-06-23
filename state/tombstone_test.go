package state_test

import (
	"context"
	"errors"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/internal/testkit"
	"github.com/ionalpha/flynn/state"
)

func TestSkillDeleteHidesFromReads(t *testing.T) {
	ctx := context.Background()
	p := state.NewMemory()
	if _, err := p.Skills().Upsert(ctx, state.Skill{Slug: "deploy", Name: "Deploy", Body: "ship it"}); err != nil {
		t.Fatal(err)
	}
	if err := p.Skills().Delete(ctx, "deploy"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := p.Skills().Get(ctx, "deploy"); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("Get after delete = %v, want ErrNotFound", err)
	}
	if got, _ := p.Skills().List(ctx, state.Scope{}); len(got) != 0 {
		t.Fatalf("List after delete = %d, want 0", len(got))
	}
	if got, _ := p.Skills().Search(ctx, "ship", 0); len(got) != 0 {
		t.Fatalf("Search after delete = %d, want 0", len(got))
	}
	// Deleting again (already a tombstone) reports not-found.
	if err := p.Skills().Delete(ctx, "deploy"); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("double delete = %v, want ErrNotFound", err)
	}
}

func TestSkillResurrectsViaUpsert(t *testing.T) {
	ctx := context.Background()
	p := state.NewMemory()
	a, _ := p.Skills().Upsert(ctx, state.Skill{Slug: "deploy", Body: "v1"})
	if err := p.Skills().Delete(ctx, "deploy"); err != nil {
		t.Fatal(err)
	}
	revived, err := p.Skills().Upsert(ctx, state.Skill{Slug: "deploy", Body: "v2"})
	if err != nil {
		t.Fatal(err)
	}
	if revived.Deleted {
		t.Fatal("an upsert over a tombstone must resurrect it")
	}
	if !a.UpdatedHLC.Before(revived.UpdatedHLC) {
		t.Fatal("the resurrection must carry a newer HLC than the original")
	}
	if got, err := p.Skills().Get(ctx, "deploy"); err != nil || got.Body != "v2" {
		t.Fatalf("resurrected skill = (%q, %v), want body v2", got.Body, err)
	}
}

func TestSessionAndMemoryDelete(t *testing.T) {
	ctx := context.Background()
	p := state.NewMemory()

	s, _ := p.Sessions().Create(ctx, state.Session{})
	if err := p.Sessions().Delete(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Sessions().Get(ctx, s.ID); !errors.Is(err, state.ErrNotFound) {
		t.Fatal("deleted session still readable")
	}
	if got, _ := p.Sessions().List(ctx); len(got) != 0 {
		t.Fatalf("session list = %d, want 0", len(got))
	}
	if err := p.Sessions().Delete(ctx, "missing"); !errors.Is(err, state.ErrNotFound) {
		t.Fatal("deleting a missing session should be ErrNotFound")
	}

	it, _ := p.Memory().Write(ctx, state.MemoryItem{Content: "secret"})
	if err := p.Memory().Delete(ctx, it.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := p.Memory().Recall(ctx, state.RecallQuery{Query: "secret"}); len(got) != 0 {
		t.Fatalf("recall after delete = %d, want 0", len(got))
	}
	if err := p.Memory().Delete(ctx, "missing"); !errors.Is(err, state.ErrNotFound) {
		t.Fatal("deleting a missing memory item should be ErrNotFound")
	}
}

// Property: once deleted, a skill never surfaces in Get, List, or Search.
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

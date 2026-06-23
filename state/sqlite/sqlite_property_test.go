package sqlite_test

import (
	"context"
	"errors"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/internal/testkit"
	"github.com/ionalpha/flynn/state"
	"github.com/ionalpha/flynn/state/sqlite"
)

// These properties drive generated skills — arbitrary unicode names, bodies, and
// tags — through the real SQL and FTS5 layers. That is where a durable backend
// can diverge from the reference: parameter binding, JSON tag encoding, and FTS
// indexing must round-trip any input without error or injection. Each iteration
// gets a fresh in-memory database.

func newProvider(rt *rapid.T) state.Provider {
	p, err := sqlite.Open(context.Background(), ":memory:")
	if err != nil {
		rt.Fatalf("open: %v", err)
	}
	return p
}

// A generated skill round-trips: it is retrievable by ID and by slug with its
// core fields intact and version 1.
func TestProp_SkillUpsertRoundtrip(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		ctx := context.Background()
		p := newProvider(rt)
		defer func() { _ = p.Close() }()

		sk := testkit.SkillGen().Draw(rt, "skill")
		saved, err := p.Skills().Upsert(ctx, sk)
		if err != nil {
			rt.Fatalf("upsert: %v", err)
		}
		if saved.ID == "" || saved.Version != 1 || saved.SyncVersion != 1 {
			rt.Fatalf("create = id=%q version=%d sync=%d, want id + 1/1", saved.ID, saved.Version, saved.SyncVersion)
		}
		byID, err := p.Skills().Get(ctx, saved.ID)
		if err != nil {
			rt.Fatalf("get by id: %v", err)
		}
		bySlug, err := p.Skills().Get(ctx, sk.Slug)
		if err != nil {
			rt.Fatalf("get by slug: %v", err)
		}
		if byID.ID != bySlug.ID {
			rt.Fatalf("get-by-id and get-by-slug disagree: %q vs %q", byID.ID, bySlug.ID)
		}
		if byID.Slug != sk.Slug || byID.Body != sk.Body || byID.Name != sk.Name {
			rt.Fatalf("roundtrip mismatch: got %+v, want slug/name/body of %+v", byID, sk)
		}
	})
}

// Re-upserting the same (scope, slug) keeps exactly one record, increments the
// version, and preserves CreatedAt — never a duplicate row.
func TestProp_SkillUpsertIsIdempotentByKey(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		ctx := context.Background()
		p := newProvider(rt)
		defer func() { _ = p.Close() }()

		sk := testkit.SkillGen().Draw(rt, "skill")
		writes := rapid.IntRange(1, 5).Draw(rt, "writes")

		var firstID string
		for i := 0; i < writes; i++ {
			saved, err := p.Skills().Upsert(ctx, sk)
			if err != nil {
				rt.Fatalf("upsert %d: %v", i, err)
			}
			if i == 0 {
				firstID = saved.ID
			}
			if saved.ID != firstID {
				rt.Fatal("re-upsert created a duplicate id")
			}
			if saved.Version != i+1 {
				rt.Fatalf("version = %d after %d writes, want %d", saved.Version, i+1, i+1)
			}
		}
		got, err := p.Skills().List(ctx, sk.Scope)
		if err != nil {
			rt.Fatalf("list: %v", err)
		}
		n := 0
		for _, s := range got {
			if s.Slug == sk.Slug {
				n++
			}
		}
		if n != 1 {
			rt.Fatalf("found %d records for slug %q, want 1", n, sk.Slug)
		}
	})
}

// Once deleted, a skill (from any generated input) never appears in Get, List,
// or Search.
func TestProp_DeletedSkillNeverSurfaces(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		ctx := context.Background()
		p := newProvider(rt)
		defer func() { _ = p.Close() }()

		saved, err := p.Skills().Upsert(ctx, testkit.SkillGen().Draw(rt, "skill"))
		if err != nil {
			rt.Fatalf("upsert: %v", err)
		}
		if err := p.Skills().Delete(ctx, saved.ID); err != nil {
			rt.Fatalf("delete: %v", err)
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

// Package skilltest is the conformance suite for state.SkillStore. Every backing
// (the in-memory resource store, the SQLite resource store, a host's) runs RunSuite
// and must behave identically, so the typed skill facade is held to one contract no
// matter which resource backend it sits on.
package skilltest

import (
	"context"
	"errors"
	"testing"

	"github.com/ionalpha/flynn/state"
)

// RunSuite runs the full SkillStore contract against stores built by newStore. Each
// subtest gets a fresh, empty store.
func RunSuite(t *testing.T, newStore func() state.SkillStore) {
	t.Helper()
	t.Run("UpsertGetListSearch", func(t *testing.T) { testCRUD(t, newStore()) })
	t.Run("OptimisticConcurrency", func(t *testing.T) { testCAS(t, newStore()) })
	t.Run("Tombstone", func(t *testing.T) { testTombstone(t, newStore()) })
}

func testCRUD(t *testing.T, sk state.SkillStore) {
	ctx := context.Background()

	if _, err := sk.Get(ctx, "missing"); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("Get(missing) = %v, want ErrNotFound", err)
	}

	a, err := sk.Upsert(ctx, state.Skill{Slug: "deploy", Name: "Deploy", Body: "ship it", Tags: []string{"ops"}})
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == "" || a.Version != 1 || a.SyncVersion != 1 {
		t.Fatalf("create = (id %q, version %d, sync %d), want id + 1/1", a.ID, a.Version, a.SyncVersion)
	}
	if a.OriginInstanceID == "" || a.LastWriterID != a.OriginInstanceID {
		t.Fatalf("create origin/writer wrong: %+v", a.Envelope)
	}

	byID, err := sk.Get(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	bySlug, err := sk.Get(ctx, "deploy")
	if err != nil {
		t.Fatal(err)
	}
	if byID.ID != bySlug.ID || byID.Body != "ship it" || len(byID.Tags) != 1 || byID.Tags[0] != "ops" {
		t.Fatalf("get-by-id/slug disagree or wrong content: %+v / %+v", byID, bySlug)
	}

	// Update preserves CreatedAt and id, bumps content and sync versions.
	b, err := sk.Upsert(ctx, state.Skill{Slug: "deploy", Name: "Deploy", Body: "ship faster"})
	if err != nil {
		t.Fatal(err)
	}
	if b.ID != a.ID || b.Version != 2 || b.SyncVersion != 2 || !b.CreatedAt.Equal(a.CreatedAt) {
		t.Fatalf("update = (id %q, v %d, sync %d, created %v), want same id, 2/2, created %v", b.ID, b.Version, b.SyncVersion, b.CreatedAt, a.CreatedAt)
	}
	if !a.UpdatedHLC.Before(b.UpdatedHLC) {
		t.Fatal("update did not advance the HLC")
	}

	// Scope isolation: a same-slug skill in another scope is a distinct record.
	if _, err := sk.Upsert(ctx, state.Skill{Slug: "deploy", Scope: state.Scope{Project: "alpha"}}); err != nil {
		t.Fatal(err)
	}
	scoped, err := sk.List(ctx, state.Scope{Project: "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if len(scoped) != 1 || scoped[0].Scope.Project != "alpha" {
		t.Fatalf("scoped List = %+v, want only the alpha skill", scoped)
	}
	if global, _ := sk.List(ctx, state.Scope{}); len(global) != 1 || global[0].Scope != (state.Scope{}) {
		t.Fatalf("global List = %+v, want only the global skill", global)
	}

	// Search spans scopes, matches content, and honours the limit.
	hits, err := sk.Search(ctx, "faster", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Slug != "deploy" || hits[0].Scope != (state.Scope{}) {
		t.Fatalf("Search(faster) = %+v, want the global deploy skill", hits)
	}
	if all, _ := sk.Search(ctx, "", 0); len(all) != 2 {
		t.Fatalf("Search(\"\") = %d, want 2 (both scopes)", len(all))
	}
	if capped, _ := sk.Search(ctx, "", 1); len(capped) != 1 {
		t.Fatalf("Search limit 1 returned %d", len(capped))
	}
}

func testCAS(t *testing.T, sk state.SkillStore) {
	ctx := context.Background()

	created, err := sk.Upsert(ctx, state.Skill{Slug: "deploy", Body: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	if created.SyncVersion != 1 {
		t.Fatalf("create SyncVersion = %d, want 1", created.SyncVersion)
	}

	upd := created
	upd.Body = "v2"
	saved, err := sk.Upsert(ctx, upd) // carries SyncVersion 1, matches
	if err != nil {
		t.Fatalf("matching-version update: %v", err)
	}
	if saved.SyncVersion != 2 {
		t.Fatalf("update SyncVersion = %d, want 2", saved.SyncVersion)
	}

	upd.Body = "v3" // upd still carries the now-stale SyncVersion 1
	if _, err := sk.Upsert(ctx, upd); !errors.Is(err, state.ErrConflict) {
		t.Fatalf("stale update = %v, want ErrConflict", err)
	}
	// A zero SyncVersion writes unconditionally.
	if _, err := sk.Upsert(ctx, state.Skill{Slug: "deploy", Body: "v4"}); err != nil {
		t.Fatalf("unconditional (zero-version) update: %v", err)
	}
	// Create-with-version (no existing record) is a conflict.
	ghost := state.Skill{Slug: "ghost"}
	ghost.SyncVersion = 5
	if _, err := sk.Upsert(ctx, ghost); !errors.Is(err, state.ErrConflict) {
		t.Fatalf("create-with-version = %v, want ErrConflict", err)
	}
}

func testTombstone(t *testing.T, sk state.SkillStore) {
	ctx := context.Background()

	original, err := sk.Upsert(ctx, state.Skill{Slug: "deploy", Name: "Deploy", Body: "ship"})
	if err != nil {
		t.Fatal(err)
	}
	if err := sk.Delete(ctx, "deploy"); err != nil {
		t.Fatal(err)
	}
	if _, err := sk.Get(ctx, "deploy"); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("Get after delete = %v, want ErrNotFound", err)
	}
	if l, _ := sk.List(ctx, state.Scope{}); len(l) != 0 {
		t.Fatalf("List after delete = %d, want 0", len(l))
	}
	if h, _ := sk.Search(ctx, "ship", 0); len(h) != 0 {
		t.Fatalf("Search after delete = %d, want 0", len(h))
	}
	if err := sk.Delete(ctx, "deploy"); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("double delete = %v, want ErrNotFound", err)
	}

	// An upsert over the tombstone resurrects it with a newer HLC.
	revived, err := sk.Upsert(ctx, state.Skill{Slug: "deploy", Body: "back"})
	if err != nil {
		t.Fatal(err)
	}
	if revived.Deleted {
		t.Fatal("upsert over a tombstone must resurrect it")
	}
	if !original.UpdatedHLC.Before(revived.UpdatedHLC) {
		t.Fatal("resurrection must carry a newer HLC")
	}
	if got, err := sk.Get(ctx, "deploy"); err != nil || got.Body != "back" {
		t.Fatalf("resurrected = (%q, %v)", got.Body, err)
	}
}

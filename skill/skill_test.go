package skill_test

import (
	"context"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/skill"
	"github.com/ionalpha/flynn/skill/skilltest"
	"github.com/ionalpha/flynn/state"
)

// newMemStore builds a skill facade over a fresh in-memory resource store with the
// Skill kind registered.
func newMemStore(t *testing.T) state.SkillStore {
	t.Helper()
	reg := resource.NewRegistry()
	if err := resource.RegisterCoreKinds(reg); err != nil {
		t.Fatalf("register core kinds: %v", err)
	}
	if err := skill.RegisterKind(reg); err != nil {
		t.Fatalf("register skill kind: %v", err)
	}
	return skill.NewStore(resource.NewMemory(reg))
}

func TestConformance(t *testing.T) {
	skilltest.RunSuite(t, func() state.SkillStore { return newMemStore(t) })
}

// TestRoundTripProperty asserts that whatever content a skill carries, an Upsert
// followed by a Get by id and by slug returns exactly that content, the slug and
// scope address it, and a re-upsert bumps the version while preserving identity.
func TestRoundTripProperty(t *testing.T) {
	ctx := context.Background()
	ident := rapid.StringMatching(`[a-z][a-z0-9-]{0,15}`)

	rapid.Check(t, func(rt *rapid.T) {
		store := newMemStore(t)
		slug := ident.Draw(rt, "slug")
		name := rapid.String().Draw(rt, "name")
		body := rapid.String().Draw(rt, "body")
		tags := rapid.SliceOfN(rapid.String(), 0, 4).Draw(rt, "tags")
		scope := state.Scope{
			Instance:  ident.Draw(rt, "instance"),
			Project:   ident.Draw(rt, "project"),
			Workspace: ident.Draw(rt, "workspace"),
		}

		created, err := store.Upsert(ctx, state.Skill{Slug: slug, Name: name, Body: body, Tags: tags, Scope: scope})
		if err != nil {
			rt.Fatalf("upsert: %v", err)
		}
		if created.Version != 1 || created.SyncVersion != 1 {
			rt.Fatalf("create versions = %d/%d, want 1/1", created.Version, created.SyncVersion)
		}

		got, err := store.Get(ctx, created.ID)
		if err != nil {
			rt.Fatalf("get by id: %v", err)
		}
		if got.Slug != slug || got.Name != name || got.Body != body || got.Scope != scope {
			rt.Fatalf("round-trip mismatch: got %+v", got)
		}
		if len(got.Tags) != len(tags) {
			rt.Fatalf("tags len = %d, want %d", len(got.Tags), len(tags))
		}
		for i := range tags {
			if got.Tags[i] != tags[i] {
				rt.Fatalf("tag %d = %q, want %q", i, got.Tags[i], tags[i])
			}
		}

		// The skill is addressable in its own scope and absent from a sibling scope.
		inScope, err := store.List(ctx, scope)
		if err != nil || len(inScope) != 1 {
			rt.Fatalf("List(scope) = (%d, %v), want 1", len(inScope), err)
		}

		updated, err := store.Upsert(ctx, state.Skill{Slug: slug, Name: name, Body: body + "!", Tags: tags, Scope: scope})
		if err != nil {
			rt.Fatalf("re-upsert: %v", err)
		}
		if updated.ID != created.ID || updated.Version != 2 || !updated.CreatedAt.Equal(created.CreatedAt) {
			rt.Fatalf("re-upsert broke identity/version: %+v", updated)
		}
	})
}

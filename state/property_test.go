package state_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/internal/testkit"
	"github.com/ionalpha/flynn/state"
)

// --- Properties (rapid + testkit generators) -------------------------------

// A skill upserted into a fresh provider is retrievable by ID and by slug, with
// its core fields intact and version 1.
func TestProp_SkillUpsertRoundtrip(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		ctx := context.Background()
		p := state.NewMemory()
		sk := testkit.SkillGen().Draw(rt, "skill")

		saved, err := p.Skills().Upsert(ctx, sk)
		if err != nil {
			rt.Fatalf("upsert: %v", err)
		}
		if saved.ID == "" || saved.Version != 1 {
			rt.Fatalf("expected an id and version 1, got id=%q version=%d", saved.ID, saved.Version)
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
// version monotonically, and preserves CreatedAt - never a duplicate.
func TestProp_SkillUpsertIsIdempotentByKey(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		ctx := context.Background()
		p := state.NewMemory()
		sk := testkit.SkillGen().Draw(rt, "skill")
		writes := rapid.IntRange(1, 6).Draw(rt, "writes")

		var firstID string
		var createdAt time.Time
		for i := range writes {
			saved, err := p.Skills().Upsert(ctx, sk)
			if err != nil {
				rt.Fatalf("upsert %d: %v", i, err)
			}
			if i == 0 {
				firstID, createdAt = saved.ID, saved.CreatedAt
			}
			if saved.ID != firstID {
				rt.Fatalf("re-upsert created a duplicate id")
			}
			if saved.Version != i+1 {
				rt.Fatalf("version = %d after %d writes, want %d", saved.Version, i+1, i+1)
			}
			if !saved.CreatedAt.Equal(createdAt) {
				rt.Fatalf("CreatedAt changed across upserts")
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

// List(scope) only ever returns records in that exact scope - scopes are
// isolated even when slugs collide.
func TestProp_SkillScopeIsolation(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		ctx := context.Background()
		p := state.NewMemory()
		a := testkit.SkillGen().Draw(rt, "a")
		b := testkit.SkillGen().Draw(rt, "b")
		if a.Scope == b.Scope {
			b.Scope.Project = a.Scope.Project + "x" // force distinct scopes
		}
		if _, err := p.Skills().Upsert(ctx, a); err != nil {
			rt.Fatalf("upsert a: %v", err)
		}
		if _, err := p.Skills().Upsert(ctx, b); err != nil {
			rt.Fatalf("upsert b: %v", err)
		}

		la, err := p.Skills().List(ctx, a.Scope)
		if err != nil {
			rt.Fatalf("list: %v", err)
		}
		for _, s := range la {
			if s.Scope != a.Scope {
				rt.Fatalf("List(scope a) returned a foreign scope %+v", s.Scope)
			}
		}
	})
}

// A skill is found by searching for its (non-empty) name, and Search respects
// the limit.
func TestProp_SkillSearchFindsAndLimits(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		ctx := context.Background()
		p := state.NewMemory()
		sk := testkit.SkillGen().Draw(rt, "skill")
		if _, err := p.Skills().Upsert(ctx, sk); err != nil {
			rt.Fatalf("upsert: %v", err)
		}

		if strings.TrimSpace(sk.Name) != "" {
			hits, err := p.Skills().Search(ctx, sk.Name, 0)
			if err != nil {
				rt.Fatalf("search: %v", err)
			}
			found := false
			for _, h := range hits {
				if h.Slug == sk.Slug {
					found = true
				}
			}
			if !found {
				rt.Fatalf("search by name %q did not find the skill", sk.Name)
			}
		}

		// An empty query matches everything; a limit of 1 caps it.
		capped, err := p.Skills().Search(ctx, "", 1)
		if err != nil {
			rt.Fatalf("search limit: %v", err)
		}
		if len(capped) > 1 {
			rt.Fatalf("limit 1 returned %d", len(capped))
		}
	})
}

// A memory item is recalled by a substring of its own content, within its scope.
func TestProp_MemoryRecallFindsByContent(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		ctx := context.Background()
		p := state.NewMemory()
		item := testkit.MemoryItemGen().Draw(rt, "item")
		if strings.TrimSpace(item.Content) == "" {
			return // nothing lexical to match on
		}
		if _, err := p.Memory().Write(ctx, item); err != nil {
			rt.Fatalf("write: %v", err)
		}
		hits, err := p.Memory().Recall(ctx, state.RecallQuery{Query: item.Content, Scope: item.Scope, Limit: 10})
		if err != nil {
			rt.Fatalf("recall: %v", err)
		}
		if len(hits) == 0 {
			rt.Fatalf("recall by exact content found nothing for %q", item.Content)
		}
	})
}

// Appending N turns yields Seq 1..N, and resuming reads them back in order.
func TestProp_SessionTurnsAreOrdered(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		ctx := context.Background()
		p := state.NewMemory()
		s, err := p.Sessions().Create(ctx, state.Session{Model: "m"})
		if err != nil {
			rt.Fatalf("create: %v", err)
		}
		n := rapid.IntRange(0, 15).Draw(rt, "n")
		for i := range n {
			got, err := p.Sessions().AppendTurn(ctx, state.Turn{SessionID: s.ID, Role: "user", Content: rapid.String().Draw(rt, "content")})
			if err != nil {
				rt.Fatalf("append %d: %v", i, err)
			}
			if got.Seq != int64(i+1) {
				rt.Fatalf("append %d Seq = %d, want %d", i, got.Seq, i+1)
			}
		}
		turns, err := p.Sessions().Turns(ctx, s.ID)
		if err != nil {
			rt.Fatalf("turns: %v", err)
		}
		if len(turns) != n {
			rt.Fatalf("read %d turns, want %d", len(turns), n)
		}
		for i, tn := range turns {
			if tn.Seq != int64(i+1) {
				rt.Fatalf("turn[%d].Seq = %d, want %d", i, tn.Seq, i+1)
			}
		}
	})
}

// A scope's ancestor chain is well-formed for every scope: it starts at the
// scope itself, ends at the global scope, never repeats, and strictly loses
// specificity as it goes. Those four together are what let a backend treat the
// chain as a ranking as well as a filter.
func TestProp_ScopeAncestorsAreAWellFormedChain(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		sc := testkit.ScopeGen().Draw(rt, "scope")
		chain := sc.Ancestors()

		if len(chain) == 0 || chain[0] != sc {
			rt.Fatalf("Ancestors(%+v) = %+v, want it to start at the scope itself", sc, chain)
		}
		if last := chain[len(chain)-1]; last != (state.Scope{}) {
			rt.Fatalf("Ancestors(%+v) ends at %+v, want the global scope", sc, last)
		}
		seen := make(map[state.Scope]bool, len(chain))
		for i, s := range chain {
			if seen[s] {
				rt.Fatalf("Ancestors(%+v) repeats %+v: %+v", sc, s, chain)
			}
			seen[s] = true
			// Strictly decreasing Depth is what makes Depth a total order over the
			// chain, which is the assumption the SQLite ORDER BY encodes as a CASE
			// over the innermost set column.
			if i > 0 && s.Depth() >= chain[i-1].Depth() {
				rt.Fatalf("Ancestors(%+v) does not lose specificity at %d: %+v", sc, i, chain)
			}
		}
	})
}

// Every ancestor encloses the scope it was derived from: an ancestor's non-empty
// fields always agree with the original's. This is the property that makes the
// chain a containment walk rather than an arbitrary set of scopes, so a widened
// recall can never reach sideways into a sibling.
func TestProp_ScopeAncestorsOnlyWiden(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		sc := testkit.ScopeGen().Draw(rt, "scope")
		for _, a := range sc.Ancestors() {
			if a.Instance != "" && a.Instance != sc.Instance {
				rt.Fatalf("ancestor %+v of %+v changed Instance", a, sc)
			}
			if a.Project != "" && a.Project != sc.Project {
				rt.Fatalf("ancestor %+v of %+v changed Project", a, sc)
			}
			if a.Workspace != "" && a.Workspace != sc.Workspace {
				rt.Fatalf("ancestor %+v of %+v changed Workspace", a, sc)
			}
		}
	})
}

// Widening only ever adds results, never removes or reorders the ones the
// unwidened recall already returned: the widened read is a superset with the
// original at its head. A caller can therefore turn IncludeAncestors on without
// re-checking what it used to get back.
func TestProp_WidenedRecallExtendsTheNarrowOne(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		ctx := context.Background()
		p := state.NewMemory()
		sc := testkit.ScopeGen().Draw(rt, "scope")

		for i, s := range sc.Ancestors() {
			for range rapid.IntRange(0, 3).Draw(rt, "writes") {
				it := state.MemoryItem{Kind: "fact", Content: "fact", Scope: s}
				if _, err := p.Memory().Write(ctx, it); err != nil {
					rt.Fatalf("write at level %d: %v", i, err)
				}
			}
		}

		narrow, err := p.Memory().Recall(ctx, state.RecallQuery{Scope: sc})
		if err != nil {
			rt.Fatalf("narrow recall: %v", err)
		}
		wide, err := p.Memory().Recall(ctx, state.RecallQuery{Scope: sc, IncludeAncestors: true})
		if err != nil {
			rt.Fatalf("wide recall: %v", err)
		}
		if len(wide) < len(narrow) {
			rt.Fatalf("widened recall returned %d, fewer than the narrow %d", len(wide), len(narrow))
		}
		for i := range narrow {
			if wide[i].ID != narrow[i].ID {
				rt.Fatalf("widened recall reordered the narrow results at %d: %q vs %q", i, wide[i].ID, narrow[i].ID)
			}
		}
	})
}

// The provider contract (CRUD, CAS, tombstones, envelope, concurrency, error
// paths) is covered once by the shared statetest.RunSuite - see
// conformance_test.go - rather than re-asserted here.

// Package memorytest is the conformance suite for state.MemoryStore. Every backing
// (the in-memory resource store, the SQLite resource store, a host's) runs RunSuite
// and must behave identically, so the typed memory facade is held to one contract no
// matter which resource backend it sits on.
package memorytest

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/ionalpha/flynn/state"
)

// RunSuite runs the full MemoryStore contract against stores built by newStore. Each
// subtest gets a fresh, empty store.
func RunSuite(t *testing.T, newStore func() state.MemoryStore) {
	t.Helper()
	t.Run("WriteRecall", func(t *testing.T) { testWriteRecall(t, newStore()) })
	t.Run("ScopeResolution", func(t *testing.T) { testScopeResolution(t, newStore()) })
	t.Run("Tombstone", func(t *testing.T) { testTombstone(t, newStore()) })
}

func testWriteRecall(t *testing.T, mem state.MemoryStore) {
	ctx := context.Background()

	a, err := mem.Write(ctx, state.MemoryItem{Kind: "fact", Content: "the user prefers Go", Source: "chat"})
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == "" || a.SyncVersion != 1 {
		t.Fatalf("write = (id %q, sync %d), want id + sync 1", a.ID, a.SyncVersion)
	}
	if a.OriginInstanceID == "" || a.LastWriterID != a.OriginInstanceID {
		t.Fatalf("write origin/writer wrong: %+v", a.Envelope)
	}
	if a.Content != "the user prefers Go" || a.Kind != "fact" || a.Source != "chat" {
		t.Fatalf("write did not round-trip content: %+v", a)
	}

	// A second write in another scope is a distinct record with its own id.
	b, err := mem.Write(ctx, state.MemoryItem{Kind: "fact", Content: "deploys go to Cloudflare", Scope: state.Scope{Project: "x"}})
	if err != nil {
		t.Fatal(err)
	}
	if b.ID == a.ID {
		t.Fatal("each write must be a distinct record")
	}

	// Recall matches content, case-insensitively, and spans scopes by default.
	hits, err := mem.Recall(ctx, state.RecallQuery{Query: "PREFERS", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Content != "the user prefers Go" {
		t.Fatalf("Recall(prefers) = %+v, want the single matching fact", hits)
	}
	// A set scope narrows the search.
	if scoped, _ := mem.Recall(ctx, state.RecallQuery{Query: "deploys", Scope: state.Scope{Project: "x"}}); len(scoped) != 1 {
		t.Fatalf("scoped Recall = %d, want 1", len(scoped))
	}
	// The narrowed scope excludes the global fact.
	if none, _ := mem.Recall(ctx, state.RecallQuery{Query: "prefers", Scope: state.Scope{Project: "x"}}); len(none) != 0 {
		t.Fatalf("scoped Recall(prefers) = %d, want 0 (it lives in the global scope)", len(none))
	}
	// An empty query matches every live item across scopes; the limit caps results.
	if all, _ := mem.Recall(ctx, state.RecallQuery{}); len(all) != 2 {
		t.Fatalf("Recall(empty) = %d, want 2 (both scopes)", len(all))
	}
	if capped, _ := mem.Recall(ctx, state.RecallQuery{Limit: 1}); len(capped) != 1 {
		t.Fatalf("Recall limit 1 returned %d", len(capped))
	}
}

// testScopeResolution pins the hierarchical read: an item written at an outer
// scope must be recallable from an inner one when the query asks to widen, and
// must stay invisible when it does not. This is the read an agent running
// workspace-under-project issues on every turn, so a backend that resolves scopes
// exactly and only exactly returns nothing useful to it.
func testScopeResolution(t *testing.T, mem state.MemoryStore) {
	ctx := context.Background()

	var (
		instance  = state.Scope{Instance: "i"}
		project   = state.Scope{Instance: "i", Project: "p"}
		workspace = state.Scope{Instance: "i", Project: "p", Workspace: "w"}
		sibling   = state.Scope{Instance: "i", Project: "p", Workspace: "other"}
	)
	for _, w := range []struct {
		scope   state.Scope
		content string
	}{
		{state.Scope{}, "global: ship on Fridays"},
		{instance, "instance: ship on Fridays"},
		{project, "project: ship on Fridays"},
		{workspace, "workspace: ship on Fridays"},
		{sibling, "sibling: ship on Fridays"},
	} {
		if _, err := mem.Write(ctx, state.MemoryItem{Kind: "fact", Content: w.content, Scope: w.scope}); err != nil {
			t.Fatalf("write at %+v: %v", w.scope, err)
		}
	}

	// Without widening, a scoped recall sees its own scope and nothing else.
	narrow, err := mem.Recall(ctx, state.RecallQuery{Query: "ship", Scope: workspace})
	if err != nil {
		t.Fatal(err)
	}
	if len(narrow) != 1 || narrow[0].Content != "workspace: ship on Fridays" {
		t.Fatalf("unwidened Recall = %s, want only the workspace's own item", contents(narrow))
	}

	// Widening walks the ancestors: own scope, project, instance, global - and
	// still never the sibling workspace, which encloses nothing.
	wide, err := mem.Recall(ctx, state.RecallQuery{Query: "ship", Scope: workspace, IncludeAncestors: true})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"workspace: ship on Fridays",
		"project: ship on Fridays",
		"instance: ship on Fridays",
		"global: ship on Fridays",
	}
	if got := contents(wide); !slices.Equal(got, want) {
		t.Fatalf("widened Recall = %v, want %v (most-specific first, no sibling scope)", got, want)
	}

	// The chain skips a level that is empty rather than inventing one: a scope with
	// no instance resolves straight through to the global scope.
	noInstance, err := mem.Recall(ctx, state.RecallQuery{
		Query: "ship", Scope: state.Scope{Project: "p"}, IncludeAncestors: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := contents(noInstance); !slices.Equal(got, []string{"global: ship on Fridays"}) {
		t.Fatalf("widened Recall from an instance-less scope = %v, want just the global item", got)
	}

	// Limit applies after resolution, so the most-specific results are the ones
	// that survive the cap rather than whichever the backend happened to scan first.
	capped, err := mem.Recall(ctx, state.RecallQuery{
		Query: "ship", Scope: workspace, IncludeAncestors: true, Limit: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := contents(capped); !slices.Equal(got, want[:2]) {
		t.Fatalf("widened Recall with limit 2 = %v, want %v", got, want[:2])
	}

	// Widening a zero scope is still the unfiltered read, not the global scope.
	all, err := mem.Recall(ctx, state.RecallQuery{Query: "ship", IncludeAncestors: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 5 {
		t.Fatalf("widened Recall with a zero scope = %d, want all 5 (a zero scope spans everything)", len(all))
	}
}

func contents(items []state.MemoryItem) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Content)
	}
	return out
}

func testTombstone(t *testing.T, mem state.MemoryStore) {
	ctx := context.Background()

	a, err := mem.Write(ctx, state.MemoryItem{Kind: "fact", Content: "ship it"})
	if err != nil {
		t.Fatal(err)
	}
	if err := mem.Delete(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := mem.Recall(ctx, state.RecallQuery{Query: "ship"}); len(got) != 0 {
		t.Fatalf("Recall after delete = %d, want 0", len(got))
	}
	if err := mem.Delete(ctx, a.ID); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("double delete = %v, want ErrNotFound", err)
	}
	if err := mem.Delete(ctx, "missing"); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("Delete(missing) = %v, want ErrNotFound", err)
	}
}

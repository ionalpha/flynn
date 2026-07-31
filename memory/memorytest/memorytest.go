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
	var (
		instance  = state.Scope{Instance: "i"}
		project   = state.Scope{Instance: "i", Project: "p"}
		workspace = state.Scope{Instance: "i", Project: "p", Workspace: "w"}
		sibling   = state.Scope{Instance: "i", Project: "p", Workspace: "other"}
	)
	write(t, mem, state.MemoryItem{Kind: "fact", Content: "global: ship", Scope: state.Scope{}})
	write(t, mem, state.MemoryItem{Kind: "fact", Content: "instance: ship", Scope: instance})
	write(t, mem, state.MemoryItem{Kind: "fact", Content: "project: ship", Scope: project})
	write(t, mem, state.MemoryItem{Kind: "fact", Content: "workspace: ship", Scope: workspace})
	write(t, mem, state.MemoryItem{Kind: "fact", Content: "sibling: ship", Scope: sibling})

	// Without widening, a scoped recall sees its own scope and nothing else.
	wantOrder(t, "unwidened recall",
		recall(t, mem, state.RecallQuery{Query: "ship", Scope: workspace}),
		"workspace: ship")

	// Widening walks the ancestors: own scope, project, instance, global - and
	// still never the sibling workspace, which encloses nothing.
	wantOrder(t, "widened recall, most-specific first and no sibling scope",
		recall(t, mem, state.RecallQuery{Query: "ship", Scope: workspace, IncludeAncestors: true}),
		"workspace: ship", "project: ship", "instance: ship", "global: ship")

	// The chain skips a level that is empty rather than inventing one: a scope with
	// no instance resolves straight through to the global scope.
	wantOrder(t, "widened recall from an instance-less scope",
		recall(t, mem, state.RecallQuery{Query: "ship", Scope: state.Scope{Project: "p"}, IncludeAncestors: true}),
		"global: ship")

	// Limit applies after resolution, so the most-specific results are the ones
	// that survive the cap rather than whichever the backend happened to scan first.
	wantOrder(t, "widened recall capped at 2",
		recall(t, mem, state.RecallQuery{Query: "ship", Scope: workspace, IncludeAncestors: true, Limit: 2}),
		"workspace: ship", "project: ship")

	// Widening a zero scope is still the unfiltered read, not the global scope.
	wantCount(t, "widened recall with a zero scope (which spans everything)",
		recall(t, mem, state.RecallQuery{Query: "ship", IncludeAncestors: true}), 5)
}

// The helpers below keep each assertion in the suite to a single call. That is
// partly readability, and partly so the failure branches - which by construction
// never run on a green build - live in one place instead of being duplicated at
// every assertion, where they would read as a wall of untested lines.

// write persists an item, failing the test if the store rejects it.
func write(t *testing.T, mem state.MemoryStore, it state.MemoryItem) state.MemoryItem {
	t.Helper()
	got, err := mem.Write(context.Background(), it)
	if err != nil {
		t.Fatalf("write %q at %+v: %v", it.Content, it.Scope, err)
	}
	return got
}

// recall runs a query, failing the test if the store errors.
func recall(t *testing.T, mem state.MemoryStore, q state.RecallQuery) []state.MemoryItem {
	t.Helper()
	got, err := mem.Recall(context.Background(), q)
	if err != nil {
		t.Fatalf("recall %+v: %v", q, err)
	}
	return got
}

// wantOrder fails unless the recall returned exactly these contents, in order.
func wantOrder(t *testing.T, what string, got []state.MemoryItem, want ...string) {
	t.Helper()
	if c := contents(got); !slices.Equal(c, want) {
		t.Fatalf("%s = %v, want %v", what, c, want)
	}
}

// wantCount fails unless the recall returned exactly n items.
func wantCount(t *testing.T, what string, got []state.MemoryItem, n int) {
	t.Helper()
	if len(got) != n {
		t.Fatalf("%s returned %d item(s) (%v), want %d", what, len(got), contents(got), n)
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

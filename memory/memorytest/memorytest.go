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
	"time"

	"github.com/ionalpha/flynn/state"
)

// RunSuite runs the full MemoryStore contract against stores built by newStore. Each
// subtest gets a fresh, empty store.
func RunSuite(t *testing.T, newStore func() state.MemoryStore) {
	t.Helper()
	t.Run("WriteRecall", func(t *testing.T) { testWriteRecall(t, newStore()) })
	t.Run("ScopeResolution", func(t *testing.T) { testScopeResolution(t, newStore()) })
	t.Run("Selectors", func(t *testing.T) { testSelectors(t, newStore()) })
	t.Run("Relevance", func(t *testing.T) { testRelevance(t, newStore()) })
	t.Run("Tombstone", func(t *testing.T) { testTombstone(t, newStore()) })
}

func testWriteRecall(t *testing.T, mem state.MemoryStore) {
	a := write(t, mem, state.MemoryItem{Kind: "fact", Content: "the user prefers Go", Source: "chat"})
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
	b := write(t, mem, state.MemoryItem{Kind: "fact", Content: "deploys go to Cloudflare", Scope: state.Scope{Project: "x"}})
	if b.ID == a.ID {
		t.Fatal("each write must be a distinct record")
	}

	// Recall matches content, case-insensitively, and spans scopes by default.
	wantOrder(t, "case-insensitive recall",
		recall(t, mem, state.RecallQuery{Query: "PREFERS", Limit: 10}),
		"the user prefers Go")
	// A set scope narrows the search, and excludes the global fact.
	wantOrder(t, "scoped recall",
		recall(t, mem, state.RecallQuery{Query: "deploys", Scope: state.Scope{Project: "x"}}),
		"deploys go to Cloudflare")
	wantCount(t, "scoped recall of a fact that lives in the global scope",
		recall(t, mem, state.RecallQuery{Query: "prefers", Scope: state.Scope{Project: "x"}}), 0)
	// An empty query matches every live item across scopes; the limit caps results.
	wantCount(t, "recall with an empty query (which spans both scopes)",
		recall(t, mem, state.RecallQuery{}), 2)
	wantCount(t, "recall capped at 1",
		recall(t, mem, state.RecallQuery{Limit: 1}), 1)
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

// wantSet fails unless the recall returned exactly these contents, in any order.
// It is the assertion for questions about which items came back, so a set answer
// cannot accidentally pin an ordering the contract leaves to CreatedAt.
func wantSet(t *testing.T, what string, got []state.MemoryItem, want ...string) {
	t.Helper()
	c := contents(got)
	slices.Sort(c)
	slices.Sort(want)
	if !slices.Equal(c, want) {
		t.Fatalf("%s = %v, want %v (in any order)", what, c, want)
	}
}

// wantCount fails unless the recall returned exactly n items.
func wantCount(t *testing.T, what string, got []state.MemoryItem, n int) {
	t.Helper()
	if len(got) != n {
		t.Fatalf("%s returned %d item(s) (%v), want %d", what, len(got), contents(got), n)
	}
}

// wantIncreasingStamps fails unless the items were stamped strictly in order. The
// time-window assertions compare against real stamps, so two writes landing on one
// clock tick would quietly weaken them rather than fail.
func wantIncreasingStamps(t *testing.T, items []state.MemoryItem) {
	t.Helper()
	for i := 1; i < len(items); i++ {
		if !items[i].CreatedAt.After(items[i-1].CreatedAt) {
			t.Fatalf("writes %d and %d share a timestamp (%v); the window assertions need distinct stamps",
				i-1, i, items[i].CreatedAt)
		}
	}
}

// testSelectors covers the non-lexical selectors: the kind filter and the
// CreatedAt window. They are what lets a caller inject preferences without
// dragging every observation along, and read "the last month" without pulling all
// of history back to filter it in the host.
func testSelectors(t *testing.T, mem state.MemoryStore) {
	// The window bounds come from what the store stamped, not from times supplied
	// here: CreatedAt is the backend's to assign on create (the resource-backed
	// facade always stamps its own), so a suite that dictated timestamps would be
	// testing one backend's write path rather than the shared read contract.
	written := make([]state.MemoryItem, 0, 4)
	for i, w := range []struct{ kind, content string }{
		{"preference", "prefers tabs"},
		{"observation", "the build was slow"},
		{"fact", "the API is versioned"},
		{"preference", "prefers dark mode"},
	} {
		if i > 0 {
			// Separate the writes past any platform's clock granularity (Windows
			// ticks at a few hundred microseconds), so the four stamps are strictly
			// increasing and the window assertions below are exact rather than
			// quietly degenerating to "everything is on one side" on a fast runner.
			time.Sleep(2 * time.Millisecond)
		}
		written = append(written, write(t, mem, state.MemoryItem{Kind: w.kind, Content: w.content}))
	}
	wantIncreasingStamps(t, written)

	// A kind filter admits only the listed kinds, and several kinds are a union.
	wantSet(t, "recall of kind preference",
		recall(t, mem, state.RecallQuery{Kinds: []string{"preference"}}),
		"prefers tabs", "prefers dark mode")
	wantCount(t, "recall of kinds preference and fact (their union)",
		recall(t, mem, state.RecallQuery{Kinds: []string{"preference", "fact"}}), 3)
	wantCount(t, "recall of an unused kind",
		recall(t, mem, state.RecallQuery{Kinds: []string{"nonesuch"}}), 0)

	// The window is half-open [Since, Until), checked against the stamps the store
	// actually assigned rather than against an assumed clock resolution.
	pivot := written[2].CreatedAt
	since := recall(t, mem, state.RecallQuery{Since: pivot})
	until := recall(t, mem, state.RecallQuery{Until: pivot})
	wantSet(t, "recall since the pivot, which is inclusive", since, sortedContents(written[2:])...)
	wantSet(t, "recall until the pivot, which is exclusive", until, sortedContents(written[:2])...)
	// The two halves partition the whole: nothing is dropped or double-counted at
	// the boundary, which is the property "half-open" is there to buy.
	if len(since)+len(until) != len(written) {
		t.Fatalf("Since and Until at one pivot returned %d + %d, want %d", len(since), len(until), len(written))
	}
	// A window that ends where it starts is empty, and a zero window is unbounded.
	wantCount(t, "recall over an empty window",
		recall(t, mem, state.RecallQuery{Since: pivot, Until: pivot}), 0)
	wantCount(t, "recall with no window",
		recall(t, mem, state.RecallQuery{}), len(written))

	// The selectors compose, and the limit caps what survives them rather than
	// capping first and letting the selectors empty the result.
	full := recall(t, mem, state.RecallQuery{Kinds: []string{"preference"}, Since: written[0].CreatedAt})
	wantCount(t, "recall of kind preference within the window", full, 2)
	capped := recall(t, mem, state.RecallQuery{Kinds: []string{"preference"}, Limit: 1})
	wantOrder(t, "recall of kind preference capped at 1", capped, full[0].Content)
	// A limit alongside a window has to cap what the window left, not cap first
	// and hand back short. A backend that pushes the limit into its query language
	// but the window into a later pass gets this wrong in exactly that way.
	wantOrder(t, "recall within the window capped at 1",
		recall(t, mem, state.RecallQuery{Kinds: []string{"preference"}, Since: written[0].CreatedAt, Limit: 1}),
		full[0].Content)
}

// testRelevance covers the score every recall now carries and the floor and
// ordering built on it. It asserts the contract's rules rather than any backend's
// numbers: a store that cannot rank scores every match 1, so these hold whether or
// not the backend has real ranking to offer.
func testRelevance(t *testing.T, mem state.MemoryStore) {
	for _, c := range []string{"alpha beta gamma", "alpha beta", "alpha"} {
		write(t, mem, state.MemoryItem{Kind: "fact", Content: c})
	}

	hits := recall(t, mem, state.RecallQuery{Query: "alpha", Order: state.OrderRelevance})
	wantCount(t, "relevance-ordered recall", hits, 3)
	for i, it := range hits {
		if it.Score <= 0 || it.Score > 1 {
			t.Fatalf("item %q scored %v, want a match in (0,1]", it.Content, it.Score)
		}
		// Relevance order is non-increasing in score, whatever the scores are.
		if i > 0 && it.Score > hits[i-1].Score {
			t.Fatalf("relevance order rose at %d: %v then %v", i, hits[i-1].Score, it.Score)
		}
	}

	// A floor above every score empties the result; the default floor of zero
	// admits everything, including from a backend that does not rank at all.
	wantCount(t, "recall with a floor above the scale",
		recall(t, mem, state.RecallQuery{Query: "alpha", MinScore: 1.0001}), 0)
	wantCount(t, "recall with the default floor",
		recall(t, mem, state.RecallQuery{Query: "alpha"}), 3)

	// Score is a read-side annotation, never part of the record: writing an item
	// that carries one does not persist it.
	if got := write(t, mem, state.MemoryItem{Kind: "fact", Content: "delta", Score: 0.5}); got.Score != 0 {
		t.Fatalf("Write returned Score %v, want it cleared", got.Score)
	}
}

func contents(items []state.MemoryItem) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Content)
	}
	return out
}

// sortedContents is contents for assertions about which items came back rather
// than in what order, so a set comparison does not accidentally pin an ordering
// the contract leaves to CreatedAt.
func sortedContents(items []state.MemoryItem) []string {
	out := contents(items)
	slices.Sort(out)
	return out
}

func testTombstone(t *testing.T, mem state.MemoryStore) {
	ctx := context.Background()

	a := write(t, mem, state.MemoryItem{Kind: "fact", Content: "ship it"})
	if err := mem.Delete(ctx, a.ID); err != nil {
		t.Fatalf("delete %q: %v", a.ID, err)
	}
	wantCount(t, "recall after delete",
		recall(t, mem, state.RecallQuery{Query: "ship"}), 0)
	if err := mem.Delete(ctx, a.ID); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("double delete = %v, want ErrNotFound", err)
	}
	if err := mem.Delete(ctx, "missing"); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("Delete(missing) = %v, want ErrNotFound", err)
	}
}

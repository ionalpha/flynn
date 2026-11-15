// Package memorytest is the conformance suite for state.MemoryStore. Every backend
// (in-memory, SQLite, a host's) runs [RunSuite], so the memory facade has one
// contract whatever stores it.
package memorytest

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/ionalpha/flynn/state"
)

// RunSuite runs the MemoryStore contract, with a fresh store from newStore for each
// subtest.
func RunSuite(t *testing.T, newStore func() state.MemoryStore) {
	t.Helper()
	t.Run("WriteRecall", func(t *testing.T) { testWriteRecall(t, newStore()) })
	t.Run("ScopeResolution", func(t *testing.T) { testScopeResolution(t, newStore()) })
	t.Run("Selectors", func(t *testing.T) { testSelectors(t, newStore()) })
	t.Run("Relevance", func(t *testing.T) { testRelevance(t, newStore()) })
	t.Run("QueryWords", func(t *testing.T) { testQueryWords(t, newStore()) })
	t.Run("Provenance", func(t *testing.T) { testProvenance(t, newStore()) })
	t.Run("Anchors", func(t *testing.T) { testAnchors(t, newStore()) })
	t.Run("Subjects", func(t *testing.T) { testSubjects(t, newStore()) })
	t.Run("Supersedes", func(t *testing.T) { testSupersedes(t, newStore()) })
	t.Run("Taint", func(t *testing.T) { testTaint(t, newStore()) })
	t.Run("Expiry", func(t *testing.T) { testExpiry(t, newStore()) })
	t.Run("Tombstone", func(t *testing.T) { testTombstone(t, newStore()) })
	t.Run("Usage", func(t *testing.T) { testUsage(t, newStore()) })
	t.Run("Promotions", func(t *testing.T) { testPromotions(t, newStore()) })
}

func testWriteRecall(t *testing.T, mem state.MemoryStore) {
	a := write(t, mem, state.MemoryItem{Kind: "fact", Content: "the user prefers Go", Sources: []string{"chat"}})
	if a.ID == "" || a.SyncVersion != 1 {
		t.Fatalf("write = (id %q, sync %d), want id + sync 1", a.ID, a.SyncVersion)
	}
	if a.OriginInstanceID == "" || a.LastWriterID != a.OriginInstanceID {
		t.Fatalf("write origin/writer wrong: %+v", a.Envelope)
	}
	if a.Content != "the user prefers Go" || a.Kind != "fact" {
		t.Fatalf("write did not round-trip content: %+v", a)
	}
	wantSources(t, "the written item", a, "chat")

	b := write(t, mem, state.MemoryItem{Kind: "fact", Content: "deploys go to Cloudflare", Scope: state.Scope{Project: "x"}})
	if b.ID == a.ID {
		t.Fatal("each write must be a distinct record")
	}

	// Recall spans scopes by default.
	wantOrder(t, "case-insensitive recall",
		recall(t, mem, state.RecallQuery{Query: "PREFERS", Limit: 10}),
		"the user prefers Go")
	wantOrder(t, "scoped recall",
		recall(t, mem, state.RecallQuery{Query: "deploys", Scope: state.Scope{Project: "x"}}),
		"deploys go to Cloudflare")
	wantCount(t, "scoped recall of a fact that lives in the global scope",
		recall(t, mem, state.RecallQuery{Query: "prefers", Scope: state.Scope{Project: "x"}}), 0)
	wantCount(t, "recall with an empty query (which spans both scopes)",
		recall(t, mem, state.RecallQuery{}), 2)
	wantCount(t, "recall capped at 1",
		recall(t, mem, state.RecallQuery{Limit: 1}), 1)
}

// testScopeResolution pins the hierarchical read: an outer item is recallable from
// an inner scope only when the query widens. An agent issues this read every turn,
// so a backend matching scopes exactly returns it nothing useful.
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

	wantOrder(t, "unwidened recall",
		recall(t, mem, state.RecallQuery{Query: "ship", Scope: workspace}),
		"workspace: ship")

	// Widening walks the ancestors and never reaches a sibling.
	wantOrder(t, "widened recall, most-specific first and no sibling scope",
		recall(t, mem, state.RecallQuery{Query: "ship", Scope: workspace, IncludeAncestors: true}),
		"workspace: ship", "project: ship", "instance: ship", "global: ship")

	// An empty level is skipped: a scope with no instance resolves to global.
	wantOrder(t, "widened recall from an instance-less scope",
		recall(t, mem, state.RecallQuery{Query: "ship", Scope: state.Scope{Project: "p"}, IncludeAncestors: true}),
		"global: ship")

	// Limit applies after resolution, so the most specific results survive the cap.
	wantOrder(t, "widened recall capped at 2",
		recall(t, mem, state.RecallQuery{Query: "ship", Scope: workspace, IncludeAncestors: true, Limit: 2}),
		"workspace: ship", "project: ship")

	// Widening a zero scope reads everything.
	wantCount(t, "widened recall with a zero scope (which spans everything)",
		recall(t, mem, state.RecallQuery{Query: "ship", IncludeAncestors: true}), 5)
}

// The helpers below make each assertion one call, so the failure branches, which
// never run on a green build, are written once.

func write(t *testing.T, mem state.MemoryStore, it state.MemoryItem) state.MemoryItem {
	t.Helper()
	got, err := mem.Write(context.Background(), it)
	if err != nil {
		t.Fatalf("write %q at %+v: %v", it.Content, it.Scope, err)
	}
	return got
}

func recall(t *testing.T, mem state.MemoryStore, q state.RecallQuery) []state.MemoryItem {
	t.Helper()
	got, err := mem.Recall(context.Background(), q)
	if err != nil {
		t.Fatalf("recall %+v: %v", q, err)
	}
	return got
}

// only returns the one item q must match.
func only(t *testing.T, mem state.MemoryStore, q state.RecallQuery) state.MemoryItem {
	t.Helper()
	got := recall(t, mem, q)
	if len(got) != 1 {
		t.Fatalf("recall %+v returned %d item(s) (%v), want exactly 1", q, len(got), contents(got))
	}
	return got[0]
}

// wantSources fails unless the item has exactly these sources, in order. No want
// means none, which differs from one empty string.
func wantSources(t *testing.T, what string, it state.MemoryItem, want ...string) {
	t.Helper()
	if len(want) == 0 && len(it.Sources) == 0 {
		return
	}
	if !slices.Equal(it.Sources, want) {
		t.Fatalf("%s has Sources %v, want %v (order preserved)", what, it.Sources, want)
	}
}

// wantExpiry also fails on an unpersisted item, because a store that dropped an
// expired write would pass every recall assertion with nothing to hide.
func wantExpiry(t *testing.T, what string, it state.MemoryItem, want time.Time) {
	t.Helper()
	if it.ID == "" {
		t.Fatalf("%s returned no record; the item must still be persisted", what)
	}
	if !it.ExpiresAt.Equal(want) {
		t.Fatalf("%s has ExpiresAt %v, want %v", what, it.ExpiresAt, want)
	}
}

func mustDelete(t *testing.T, mem state.MemoryStore, id, why string) {
	t.Helper()
	if err := mem.Delete(context.Background(), id); err != nil {
		t.Fatalf("delete %q = %v, want it to succeed: %s", id, err, why)
	}
}

func wantOrder(t *testing.T, what string, got []state.MemoryItem, want ...string) {
	t.Helper()
	if c := contents(got); !slices.Equal(c, want) {
		t.Fatalf("%s = %v, want %v", what, c, want)
	}
}

// wantSet ignores order, so a membership check does not pin an order the contract
// leaves to CreatedAt.
func wantSet(t *testing.T, what string, got []state.MemoryItem, want ...string) {
	t.Helper()
	c := contents(got)
	slices.Sort(c)
	slices.Sort(want)
	if !slices.Equal(c, want) {
		t.Fatalf("%s = %v, want %v (in any order)", what, c, want)
	}
}

func wantCount(t *testing.T, what string, got []state.MemoryItem, n int) {
	t.Helper()
	if len(got) != n {
		t.Fatalf("%s returned %d item(s) (%v), want %d", what, len(got), contents(got), n)
	}
}

// wantIncreasingStamps fails on a shared timestamp, which would weaken the window
// assertions without failing them.
func wantIncreasingStamps(t *testing.T, items []state.MemoryItem) {
	t.Helper()
	for i := 1; i < len(items); i++ {
		if !items[i].CreatedAt.After(items[i-1].CreatedAt) {
			t.Fatalf("writes %d and %d share a timestamp (%v); the window assertions need distinct stamps",
				i-1, i, items[i].CreatedAt)
		}
	}
}

// testSelectors covers the kind filter and the CreatedAt window, which let a caller
// read one kind or one period without filtering all history in the host.
func testSelectors(t *testing.T, mem state.MemoryStore) {
	// The bounds come from the store's stamps, because CreatedAt is the backend's to
	// assign.
	written := make([]state.MemoryItem, 0, 4)
	for i, w := range []struct{ kind, content string }{
		{"preference", "prefers tabs"},
		{"observation", "the build was slow"},
		{"fact", "the API is versioned"},
		{"preference", "prefers dark mode"},
	} {
		if i > 0 {
			// Exceed clock granularity (a few hundred microseconds on Windows), so the
			// stamps strictly increase on a fast runner.
			time.Sleep(2 * time.Millisecond)
		}
		written = append(written, write(t, mem, state.MemoryItem{Kind: w.kind, Content: w.content}))
	}
	wantIncreasingStamps(t, written)

	// Several kinds are a union.
	wantSet(t, "recall of kind preference",
		recall(t, mem, state.RecallQuery{Kinds: []string{"preference"}}),
		"prefers tabs", "prefers dark mode")
	wantCount(t, "recall of kinds preference and fact (their union)",
		recall(t, mem, state.RecallQuery{Kinds: []string{"preference", "fact"}}), 3)
	wantCount(t, "recall of an unused kind",
		recall(t, mem, state.RecallQuery{Kinds: []string{"nonesuch"}}), 0)

	// The window is half-open [Since, Until).
	pivot := written[2].CreatedAt
	since := recall(t, mem, state.RecallQuery{Since: pivot})
	until := recall(t, mem, state.RecallQuery{Until: pivot})
	wantSet(t, "recall since the pivot, which is inclusive", since, sortedContents(written[2:])...)
	wantSet(t, "recall until the pivot, which is exclusive", until, sortedContents(written[:2])...)
	// The halves partition the whole, with nothing dropped or doubled at the pivot.
	if len(since)+len(until) != len(written) {
		t.Fatalf("Since and Until at one pivot returned %d + %d, want %d", len(since), len(until), len(written))
	}
	// A window that ends where it starts is empty, and a zero window is unbounded.
	wantCount(t, "recall over an empty window",
		recall(t, mem, state.RecallQuery{Since: pivot, Until: pivot}), 0)
	wantCount(t, "recall with no window",
		recall(t, mem, state.RecallQuery{}), len(written))

	// The limit caps what survives the selectors.
	full := recall(t, mem, state.RecallQuery{Kinds: []string{"preference"}, Since: written[0].CreatedAt})
	wantCount(t, "recall of kind preference within the window", full, 2)
	capped := recall(t, mem, state.RecallQuery{Kinds: []string{"preference"}, Limit: 1})
	wantOrder(t, "recall of kind preference capped at 1", capped, full[0].Content)
	// A backend applying the limit in its query and the window afterwards fails
	// this.
	wantOrder(t, "recall within the window capped at 1",
		recall(t, mem, state.RecallQuery{Kinds: []string{"preference"}, Since: written[0].CreatedAt, Limit: 1}),
		full[0].Content)
}

// testRelevance covers the score, floor and ordering. It asserts rules, not numbers,
// because a store that cannot rank scores every match 1.
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
		if i > 0 && it.Score > hits[i-1].Score {
			t.Fatalf("relevance order rose at %d: %v then %v", i, hits[i-1].Score, it.Score)
		}
	}

	// The default floor of zero admits everything, even from a backend that does not
	// rank.
	wantCount(t, "recall with a floor above the scale",
		recall(t, mem, state.RecallQuery{Query: "alpha", MinScore: 1.0001}), 0)
	wantCount(t, "recall with the default floor",
		recall(t, mem, state.RecallQuery{Query: "alpha"}), 3)

	// Score is read-side only and is not persisted.
	if got := write(t, mem, state.MemoryItem{Kind: "fact", Content: "delta", Score: 0.5}); got.Score != 0 {
		t.Fatalf("Write returned Score %v, want it cleared", got.Score)
	}
}

// testQueryWords holds a store to matching a question by its words, because a
// phrase match returns nothing for different phrasing and an empty answer looks
// like an empty store. The rules are recallplan.Plan's: all words first, and only
// when nothing has them all, items with some, full matches ranked first.
func testQueryWords(t *testing.T, mem state.MemoryStore) {
	decision := write(t, mem, state.MemoryItem{Kind: "fact", Content: "We decided to keep the database on PostgreSQL after the migration review"})
	write(t, mem, state.MemoryItem{Kind: "fact", Content: "The review of the deploy pipeline is on Fridays"})
	backups := write(t, mem, state.MemoryItem{Kind: "fact", Content: "Database backups run nightly"})

	// The words are in the item out of order. The other items each hold one content
	// word and must stay out, because widening only applies to an empty answer.
	got := recall(t, mem, state.RecallQuery{Query: "Which database did we keep after the review?"})
	wantSet(t, "a question whose words are in the item, not adjacent", got, decision.Content)

	// Nothing holds all four words, so the recall widens, fuller matches first.
	got = recall(t, mem, state.RecallQuery{Query: "database nightly backups restore", Order: state.OrderRelevance})
	wantOrder(t, "a question no item answers in full", got, backups.Content, decision.Content)
	if got[0].Score < got[1].Score {
		t.Fatalf("the fuller match scored %v below the partial one at %v", got[0].Score, got[1].Score)
	}

	wantCount(t, "a question none of whose words appear", recall(t, mem, state.RecallQuery{Query: "kubernetes terraform"}), 0)

	// Full-text query syntax is matched as text, without error or matching
	// everything.
	for _, q := range []string{`database AND`, `NEAR(database`, `"database`, `data*`, `-database`, `title:database`, `???`} {
		got, err := mem.Recall(context.Background(), state.RecallQuery{Query: q})
		if err != nil {
			t.Fatalf("recall of %q: %v", q, err)
		}
		if len(got) > 2 {
			t.Fatalf("recall of %q returned %d items (%v), want it matched as text", q, len(got), contents(got))
		}
	}
}

// testProvenance pins provenance as a list, so purging a compromised source finds
// the items where it was one of several.
func testProvenance(t *testing.T, mem state.MemoryStore) {
	many := []string{"user:operator", "tool:web-fetch", "agent:run-7"}
	// The writer's order survives storage.
	wantSources(t, "Write of a multi-source item",
		write(t, mem, state.MemoryItem{Kind: "fact", Content: "distilled from three inputs", Sources: many}), many...)
	wantSources(t, "recall of the multi-source item",
		only(t, mem, state.RecallQuery{Query: "distilled"}), many...)

	// No provenance must stay distinguishable from one source "".
	wantSources(t, "Write of an item with no sources",
		write(t, mem, state.MemoryItem{Kind: "fact", Content: "no recorded origin"}))
	wantSources(t, "recall of the item with no provenance",
		only(t, mem, state.RecallQuery{Query: "recorded"}))

	wantSources(t, "Write of a single-source item",
		write(t, mem, state.MemoryItem{Kind: "fact", Content: "single origin", Sources: []string{"chat"}}), "chat")
}

// testTaint pins the taint flag as stored. A backend that dropped it would let the
// digest push a laundered fact, with no error anywhere.
//
// The items carry agent and user sources, so a backend that recomputed taint from
// sources fails.
func testTaint(t *testing.T, mem state.MemoryStore) {
	clean := write(t, mem, state.MemoryItem{
		Kind: "fact", Content: "an untainted note", Sources: []string{"agent:run-1"},
	})
	if clean.Tainted {
		t.Fatalf("Write of an unmarked item returned Tainted, want it clear: %+v", clean)
	}
	laundered := write(t, mem, state.MemoryItem{
		Kind: "fact", Content: "a conclusion drawn from a fetched page",
		Sources: []string{"agent:run-1"}, Tainted: true,
	})
	if !laundered.Tainted {
		t.Fatalf("Write of a tainted item returned it clean: %+v", laundered)
	}
	if got := only(t, mem, state.RecallQuery{Query: "conclusion"}); !got.Tainted {
		t.Fatalf("recall of the tainted item returned it clean: %+v", got)
	}
	if got := only(t, mem, state.RecallQuery{Query: "untainted note"}); got.Tainted {
		t.Fatalf("recall of the clean item returned it tainted: %+v", got)
	}

	// A tainted item is still recallable, unlike an ingest refusal.
	wantCount(t, "recall spanning both items", recall(t, mem, state.RecallQuery{}), 2)

	// Taint beats provenance: a user source does not clear it, or the laundered item
	// returns to the push set.
	fromUser := write(t, mem, state.MemoryItem{
		Kind: "preference", Content: "operator said so, in a poisoned context",
		Sources: []string{"user:operator"}, Tainted: true,
	})
	if !fromUser.Tainted {
		t.Fatalf("a user-sourced write shed its taint: %+v", fromUser)
	}
	if got := only(t, mem, state.RecallQuery{Query: "operator said"}); !got.Tainted {
		t.Fatalf("recall of the user-sourced tainted item returned it clean: %+v", got)
	}
}

// testAnchors pins anchors as opaque refs: stored, matched, never resolved. An
// anchor pointing at nothing is valid. A store that dropped unresolvable refs would
// need a resolver for a namespace it does not own, and would delete facts whose
// subject it cannot see.
func testAnchors(t *testing.T, mem state.MemoryStore) {
	ctx := context.Background()
	widget := state.Anchor{Kind: "widget", ID: "w-1"}
	gadget := state.Anchor{Kind: "gadget", ID: "g-9"}
	// The same id under another kind, so matching on id alone fails.
	widgetShapedGadget := state.Anchor{Kind: "gadget", ID: "w-1"}

	both := write(t, mem, state.MemoryItem{
		Kind: "fact", Content: "spans two subjects", Anchors: []state.Anchor{gadget, widget},
	})
	write(t, mem, state.MemoryItem{
		Kind: "fact", Content: "about the widget only", Anchors: []state.Anchor{widget},
	})
	write(t, mem, state.MemoryItem{Kind: "fact", Content: "about nothing in particular"})

	// Every backend returns anchors sorted and deduplicated, so one set encodes one
	// way.
	wantAnchors(t, "Write of a two-anchor item", both, gadget, widget)
	wantAnchors(t, "recall of the two-anchor item",
		only(t, mem, state.RecallQuery{Query: "spans"}), gadget, widget)
	wantAnchors(t, "Write of an item with a duplicated anchor",
		write(t, mem, state.MemoryItem{
			Kind:    "fact",
			Content: "written with a repeat",
			Anchors: []state.Anchor{widget, gadget, widget},
		}), gadget, widget)
	wantAnchors(t, "Write of an unanchored item",
		write(t, mem, state.MemoryItem{Kind: "fact", Content: "unanchored"}))

	// The ride-along read: a ref and no query returns that ref's facts only.
	wantSet(t, "recall by anchor with no query",
		recall(t, mem, state.RecallQuery{Anchors: []state.Anchor{widget}}),
		"about the widget only", "spans two subjects", "written with a repeat")

	// Anchors match any-of, and an item matching several returns once.
	wantSet(t, "recall by either of two anchors",
		recall(t, mem, state.RecallQuery{Anchors: []state.Anchor{widget, gadget}}),
		"about the widget only", "spans two subjects", "written with a repeat")

	// Kind is matched too, because it keeps two systems' ids apart.
	wantCount(t, "recall by a matching id under the wrong kind",
		recall(t, mem, state.RecallQuery{Anchors: []state.Anchor{widgetShapedGadget}}), 0)

	// An unknown anchor is empty, without error, because the store cannot tell a ref
	// that never existed from one whose referent is gone.
	wantCount(t, "recall by an unknown anchor",
		recall(t, mem, state.RecallQuery{Anchors: []state.Anchor{{Kind: "widget", ID: "gone"}}}), 0)

	// Anchors narrow along with the other selectors.
	wantSet(t, "recall by anchor and content",
		recall(t, mem, state.RecallQuery{Query: "widget", Anchors: []state.Anchor{widget}}),
		"about the widget only")
	wantCount(t, "recall by anchor and a non-matching kind",
		recall(t, mem, state.RecallQuery{Anchors: []state.Anchor{widget}, Kinds: []string{"preference"}}), 0)

	// Anchors say what an item is about and sources where it came from. A backend
	// folding one into the other passes every assertion above.
	mixed := write(t, mem, state.MemoryItem{
		Kind:    "fact",
		Content: "learned from a tool while working on the widget",
		Sources: []string{"tool:web-fetch"},
		Anchors: []state.Anchor{widget},
	})
	wantSources(t, "Write of an item with both", mixed, "tool:web-fetch")
	wantAnchors(t, "Write of an item with both", mixed, widget)

	// A tombstone leaves an anchored recall too, which is a separate index on some
	// backends.
	mustDelete(t, mem, both.ID, "the anchored item was just written and is live")
	wantSet(t, "recall by anchor after a delete",
		recall(t, mem, state.RecallQuery{Anchors: []state.Anchor{widget}}),
		"about the widget only", "written with a repeat",
		"learned from a tool while working on the widget")

	// A write checks shape only. Half an anchor can never match, so it is refused.
	for _, bad := range []state.Anchor{{Kind: "widget"}, {ID: "w-1"}, {}} {
		_, err := mem.Write(ctx, state.MemoryItem{
			Kind: "fact", Content: "malformed anchor", Anchors: []state.Anchor{bad},
		})
		if !errors.Is(err, state.ErrInvalid) {
			t.Fatalf("Write with anchor %+v = %v, want ErrInvalid", bad, err)
		}
	}
	wantCount(t, "recall after the rejected writes",
		recall(t, mem, state.RecallQuery{Query: "malformed"}), 0)
}

// testSubjects pins the subject as a stored, normalized key. Spellings of one topic
// share a key, and the filter matches the key exactly, so a store searching content
// fails on items that never mention their subject.
func testSubjects(t *testing.T, mem state.MemoryStore) {
	ctx := context.Background()

	// Three spellings of one topic must store identically, or a policy reading by
	// subject sees three chains.
	for _, spelling := range []string{"db-choice", "DB Choice", "db_choice"} {
		got := write(t, mem, state.MemoryItem{
			Kind: "decision", Content: "wrote it as " + spelling, Subject: spelling,
		})
		if got.Subject != "db-choice" {
			t.Fatalf("Write of subject %q stored %q, want %q", spelling, got.Subject, "db-choice")
		}
	}
	// The content omits "db-choice", so a lexical filter misses it.
	write(t, mem, state.MemoryItem{
		Kind: "decision", Content: "we are going with Postgres", Subject: "db-choice",
	})
	write(t, mem, state.MemoryItem{
		Kind: "decision", Content: "the queue runs on NATS", Subject: "queue-choice",
	})
	// An unsubjected item must not match another key.
	write(t, mem, state.MemoryItem{Kind: "observation", Content: "the build was slow today"})

	wantSet(t, "recall by subject with no query",
		recall(t, mem, state.RecallQuery{Subjects: []string{"db-choice"}}),
		"wrote it as db-choice", "wrote it as DB Choice", "wrote it as db_choice",
		"we are going with Postgres")
	wantSet(t, "recall by either of two subjects",
		recall(t, mem, state.RecallQuery{Subjects: []string{"queue-choice", "nothing-here"}}),
		"the queue runs on NATS")
	wantCount(t, "recall by a subject nothing was written under",
		recall(t, mem, state.RecallQuery{Subjects: []string{"nothing-here"}}), 0)
	// The filter is exact, so an un-normalized spelling matches nothing.
	wantCount(t, "recall by an un-normalized subject",
		recall(t, mem, state.RecallQuery{Subjects: []string{"DB Choice"}}), 0)
	// The empty subject matches only when asked for.
	wantSet(t, "recall by the empty subject",
		recall(t, mem, state.RecallQuery{Subjects: []string{""}}),
		"the build was slow today")

	wantSet(t, "recall by subject and content",
		recall(t, mem, state.RecallQuery{Subjects: []string{"db-choice"}, Query: "Postgres"}),
		"we are going with Postgres")
	wantCount(t, "recall by subject and a non-matching kind",
		recall(t, mem, state.RecallQuery{Subjects: []string{"db-choice"}, Kinds: []string{"observation"}}), 0)

	// A tombstoned item leaves the series, so a policy does not weigh a retired fact.
	retired := write(t, mem, state.MemoryItem{
		Kind: "decision", Content: "an earlier answer", Subject: "db-choice",
	})
	mustDelete(t, mem, retired.ID, "the subjected item was just written and is live")
	wantCount(t, "recall by subject after a delete",
		recall(t, mem, state.RecallQuery{Subjects: []string{"db-choice"}}), 4)

	// An unkeyable subject is refused, so the writer does not believe it has a key.
	for _, bad := range []string{"---", ".", " ?! "} {
		_, err := mem.Write(ctx, state.MemoryItem{
			Kind: "decision", Content: "unkeyable subject", Subject: bad,
		})
		if !errors.Is(err, state.ErrInvalid) {
			t.Fatalf("Write with subject %q = %v, want ErrInvalid", bad, err)
		}
	}
	wantCount(t, "recall after the rejected writes",
		recall(t, mem, state.RecallQuery{Query: "unkeyable"}), 0)
}

// testSupersedes pins supersession as a link only: a replacement does not retire
// what it replaced. An inferred tombstone would stop a host keeping both facts live,
// and would skip the audit trail Delete leaves.
func testSupersedes(t *testing.T, mem state.MemoryStore) {
	ctx := context.Background()
	first := write(t, mem, state.MemoryItem{
		Kind: "decision", Content: "we are going with MySQL", Subject: "db-choice",
	})
	second := write(t, mem, state.MemoryItem{
		Kind: "decision", Content: "we are going with Postgres", Subject: "db-choice",
		Supersedes: []string{first.ID},
	})
	if !slices.Equal(second.Supersedes, []string{first.ID}) {
		t.Fatalf("Write supersedes = %v, want [%s]", second.Supersedes, first.ID)
	}
	wantSupersedes(t, "recall of the superseding item",
		only(t, mem, state.RecallQuery{Query: "Postgres"}), first.ID)
	// Both stay live.
	wantCount(t, "recall of the subject after a supersession",
		recall(t, mem, state.RecallQuery{Subjects: []string{"db-choice"}}), 2)
	wantSupersedes(t, "recall of the superseded item",
		only(t, mem, state.RecallQuery{Query: "MySQL"}))

	// Sorted and deduplicated, so distilling one series twice encodes identically.
	a := write(t, mem, state.MemoryItem{Kind: "episode", Content: "failure one", Subject: "flaky-deploy"})
	b := write(t, mem, state.MemoryItem{Kind: "episode", Content: "failure two", Subject: "flaky-deploy"})
	ids := []string{a.ID, b.ID}
	slices.Sort(ids)
	lesson := write(t, mem, state.MemoryItem{
		Kind: "lesson", Content: "the deploy fails when the cache is cold", Subject: "flaky-deploy",
		Supersedes: []string{b.ID, a.ID, b.ID},
	})
	wantSupersedes(t, "Write of a lesson distilled from two episodes", lesson, ids...)

	// An id of a deleted item stays as a dead link, because ids are not resolved.
	mustDelete(t, mem, a.ID, "the episode was just written and is live")
	wantSupersedes(t, "recall of a lesson whose episode was deleted",
		only(t, mem, state.RecallQuery{Query: "cold"}), ids...)

	// Self-supersession is refused, because walking the chain would never end.
	self := "mem-self-loop"
	_, err := mem.Write(ctx, state.MemoryItem{
		ID: self, Kind: "decision", Content: "supersedes itself", Supersedes: []string{self},
	})
	if !errors.Is(err, state.ErrInvalid) {
		t.Fatalf("Write superseding itself = %v, want ErrInvalid", err)
	}
	_, err = mem.Write(ctx, state.MemoryItem{
		Kind: "decision", Content: "supersedes a blank", Supersedes: []string{""},
	})
	if !errors.Is(err, state.ErrInvalid) {
		t.Fatalf("Write superseding a blank id = %v, want ErrInvalid", err)
	}
	wantCount(t, "recall after the rejected writes",
		recall(t, mem, state.RecallQuery{Query: "supersedes"}), 0)
}

// wantSupersedes compares in canonical order; no want means none.
func wantSupersedes(t *testing.T, what string, it state.MemoryItem, want ...string) {
	t.Helper()
	if len(want) == 0 && len(it.Supersedes) == 0 {
		return
	}
	if !slices.Equal(it.Supersedes, want) {
		t.Fatalf("%s: supersedes = %v, want %v", what, it.Supersedes, want)
	}
}

// wantAnchors compares in canonical order; no want means none.
func wantAnchors(t *testing.T, what string, it state.MemoryItem, want ...state.Anchor) {
	t.Helper()
	if len(want) == 0 && len(it.Anchors) == 0 {
		return
	}
	if !slices.Equal(it.Anchors, want) {
		t.Fatalf("%s: anchors = %+v, want %+v", what, it.Anchors, want)
	}
}

// testExpiry pins MemoryItem.ExpiresAt across every recall shape, because a backend
// with a fast path for the startup read can serve expired memory from that path
// alone.
func testExpiry(t *testing.T, mem state.MemoryStore) {
	scope := state.Scope{Project: "p"}
	// Times are relative to a store-assigned stamp, because recall judges expiry by
	// the backend's clock. An hour either side makes test duration irrelevant.
	anchor := write(t, mem, state.MemoryItem{
		Kind: "fact", Content: "permanent credential", Scope: scope,
	}).CreatedAt
	past := anchor.Add(-time.Hour)
	future := anchor.Add(time.Hour)

	dead := write(t, mem, state.MemoryItem{
		Kind: "fact", Content: "rotated credential", Scope: scope, ExpiresAt: past,
	})
	write(t, mem, state.MemoryItem{
		Kind: "fact", Content: "live credential", Scope: scope, ExpiresAt: future,
	})

	// Write accepts an expired item with the field intact.
	wantExpiry(t, "Write of an already-expired item", dead, past)

	live := []string{"live credential", "permanent credential"}
	// The startup read, the most likely to have its own path.
	wantSet(t, "scoped recall with no query", recall(t, mem, state.RecallQuery{Scope: scope}), live...)
	// The lexical read is a separate query on a full-text backend.
	wantSet(t, "recall by content", recall(t, mem, state.RecallQuery{Query: "credential"}), live...)
	wantSet(t, "unfiltered recall", recall(t, mem, state.RecallQuery{}), live...)
	// With a kind filter, which some backends push into the query and some do not.
	wantSet(t, "recall of kind fact", recall(t, mem, state.RecallQuery{Kinds: []string{"fact"}}), live...)
	// Widened over a scope chain, the shape that cannot use a prepared statement.
	wantSet(t, "widened recall",
		recall(t, mem, state.RecallQuery{Scope: scope, IncludeAncestors: true}), live...)
	wantSet(t, "relevance-ordered recall",
		recall(t, mem, state.RecallQuery{Query: "credential", Order: state.OrderRelevance}), live...)
	// The limit caps what survived expiry; capping first would return one item.
	wantCount(t, "recall capped at 2", recall(t, mem, state.RecallQuery{Scope: scope, Limit: 2}), 2)

	// Expiry only hides an item from recall, so Delete still finds it.
	mustDelete(t, mem, dead.ID, "expiry is not deletion, so an expired item is still there to tombstone")
}

// testUsage pins push and use counting and the organic and primed split. A backend
// that drops a use's origin, or counts a push as a read, fails.
func testUsage(t *testing.T, mem state.MemoryStore) {
	ctx := context.Background()
	a := write(t, mem, state.MemoryItem{Kind: "fact", Content: "the deploy target is Cloudflare"})
	b := write(t, mem, state.MemoryItem{Kind: "preference", Content: "the user prefers Go"})

	// An untouched item has no row, so it differs from one pushed and ignored.
	wantUsageCount(t, "before any read", wantUsage(t, "before any read", mem, nil), 0)

	// A push counts once per item, however often it is named.
	mustRecordPush(t, mem, []string{a.ID, b.ID, a.ID})
	rows := wantUsage(t, "after one push", mem, []string{a.ID, b.ID})
	wantUsageCount(t, "after one push", rows, 2)
	for _, u := range rows {
		wantCounts(t, "after one push", u, 1, 0, 0)
		wantStamps(t, "after one push", u, true, false)
	}

	// A primed use clears Ignored but has its own counter, because it says more
	// about the push than the item.
	mustRecordUse(t, mem, a.ID, state.UsagePrimed)
	mustRecordUse(t, mem, b.ID, state.UsageOrganic)
	mustRecordUse(t, mem, b.ID, state.UsageOrganic)
	ua := wantOneUsage(t, "after a primed use", mem, a.ID)
	wantCounts(t, "after a primed use", ua, 1, 0, 1)
	wantStamps(t, "after a primed use", ua, true, true)
	wantCounts(t, "after two organic uses", wantOneUsage(t, "after two organic uses", mem, b.ID), 1, 2, 0)

	// An unknown origin is refused and records nothing.
	wantErrIs(t, "RecordUse with no origin",
		mem.RecordUse(ctx, a.ID, state.UsageOrigin("")), state.ErrInvalid)
	wantErrIs(t, "RecordUse with an unknown origin",
		mem.RecordUse(ctx, a.ID, state.UsageOrigin("guessed")), state.ErrInvalid)

	// One unknown id fails the whole set, so a stale digest entry surfaces.
	wantErrIs(t, "RecordPush over an unknown id",
		mem.RecordPush(ctx, []string{a.ID, "missing"}), state.ErrNotFound)
	wantErrIs(t, "RecordUse of an unknown id",
		mem.RecordUse(ctx, "missing", state.UsageOrganic), state.ErrNotFound)
	wantCounts(t, "after the rejected push", wantOneUsage(t, "after the rejected push", mem, a.ID), 1, 0, 1)

	// An empty push succeeds, because an empty digest has nothing to report.
	wantErrIs(t, "RecordPush(nil)", mem.RecordPush(ctx, nil), nil)

	// Usage outlives the item, so a curator sees what was shown before retirement.
	// A tombstoned item cannot be pushed.
	mustDelete(t, mem, b.ID, "the item was just written and is live")
	wantErrIs(t, "RecordPush of a tombstoned item",
		mem.RecordPush(ctx, []string{b.ID}), state.ErrNotFound)
	wantCounts(t, "after the item was deleted", wantOneUsage(t, "after the item was deleted", mem, b.ID), 1, 2, 0)

	// An empty id list is the fleet-wide read, ordered by item then instance.
	all := wantUsage(t, "the unfiltered read", mem, nil)
	wantUsageCount(t, "the unfiltered read", all, 2)
	if all[0].MemoryID > all[1].MemoryID {
		t.Fatalf("unfiltered usage is not ordered by item: %+v", all)
	}
}

// testPromotions pins one revisable, attributed decision per item, refused when it
// could not be audited. An unreviewed item has no row, so an audit can tell silence
// from a refusal.
func testPromotions(t *testing.T, mem state.MemoryStore) {
	a := write(t, mem, state.MemoryItem{Kind: "fact", Content: "the deploy target is Cloudflare"})
	b := write(t, mem, state.MemoryItem{Kind: "preference", Content: "the user prefers Go"})

	wantPromotionCount(t, "before any review", wantPromotions(t, "before any review", mem, nil), 0)

	got := mustPromote(t, mem, state.PromotionDecision{
		MemoryID: a.ID, Promoted: true, By: "user:operator", Reason: "checked against the runbook",
	})
	if !got.Promoted || got.By != "user:operator" || got.Reason != "checked against the runbook" {
		t.Fatalf("Promote returned %+v, want the decision as made", got)
	}
	if got.MemoryID != a.ID || got.DecidedAt.IsZero() || got.SyncVersion == 0 || got.OriginInstanceID == "" {
		t.Fatalf("Promote returned %+v, want it stamped for item %s", got, a.ID)
	}
	if only := wantOnePromotion(t, "after one promotion", mem, a.ID); !only.Promoted {
		t.Fatalf("the promotion did not survive the round trip: %+v", only)
	}
	wantPromotionCount(t, "the unreviewed item", wantPromotions(t, "the unreviewed item", mem, []string{b.ID}), 0)

	// A revision replaces the row and advances its version.
	revoked := mustPromote(t, mem, state.PromotionDecision{MemoryID: a.ID, By: "user:operator", Reason: "stale"})
	if revoked.Promoted || revoked.Reason != "stale" {
		t.Fatalf("revocation returned %+v, want it not promoted", revoked)
	}
	if revoked.SyncVersion <= got.SyncVersion {
		t.Fatalf("revocation version %d did not advance past %d", revoked.SyncVersion, got.SyncVersion)
	}
	wantPromotionCount(t, "after the revocation", wantPromotions(t, "after the revocation", mem, nil), 1)
	if now := wantOnePromotion(t, "after the revocation", mem, a.ID); now.Promoted {
		t.Fatalf("the revoked item reads as promoted: %+v", now)
	}
	// The promoted set excludes revoked and unreviewed items.
	if set := state.PromotedSet(wantPromotions(t, "the promoted set", mem, nil)); len(set) != 0 {
		t.Fatalf("PromotedSet = %v, want nothing promoted", set)
	}

	wantPromoteErr(t, "a promotion with no reviewer", mem,
		state.PromotionDecision{MemoryID: a.ID, Promoted: true}, state.ErrInvalid)
	wantPromoteErr(t, "a promotion with no item", mem,
		state.PromotionDecision{Promoted: true, By: "user:operator"}, state.ErrInvalid)
	wantPromoteErr(t, "a promotion of an unknown item", mem,
		state.PromotionDecision{MemoryID: "missing", Promoted: true, By: "user:operator"}, state.ErrNotFound)

	// A decision outlives its item, which cannot then be promoted again.
	mustPromote(t, mem, state.PromotionDecision{MemoryID: b.ID, Promoted: true, By: "user:operator"})
	mustDelete(t, mem, b.ID, "the item was just written and is live")
	wantPromoteErr(t, "a promotion of a tombstoned item", mem,
		state.PromotionDecision{MemoryID: b.ID, Promoted: true, By: "user:operator"}, state.ErrNotFound)
	if kept := wantOnePromotion(t, "after the item was deleted", mem, b.ID); !kept.Promoted {
		t.Fatalf("the decision did not outlive its item: %+v", kept)
	}

	// The digest reads its whole candidate set in one call.
	pair := wantPromotions(t, "a read of both items", mem, []string{a.ID, b.ID})
	wantPromotionCount(t, "a read of both items", pair, 2)

	all := wantPromotions(t, "the unfiltered read", mem, nil)
	wantPromotionCount(t, "the unfiltered read", all, 2)
	if all[0].MemoryID > all[1].MemoryID {
		t.Fatalf("unfiltered promotions are not ordered by item: %+v", all)
	}
}

func mustPromote(t *testing.T, mem state.MemoryStore, d state.PromotionDecision) state.MemoryPromotion {
	t.Helper()
	got, err := mem.Promote(context.Background(), d)
	if err != nil {
		t.Fatalf("Promote(%+v): %v", d, err)
	}
	return got
}

// wantPromoteErr also fails if a refused decision left a row.
func wantPromoteErr(t *testing.T, what string, mem state.MemoryStore, d state.PromotionDecision, want error) {
	t.Helper()
	before := wantPromotions(t, what, mem, nil)
	_, err := mem.Promote(context.Background(), d)
	wantErrIs(t, what, err, want)
	if after := wantPromotions(t, what, mem, nil); len(after) != len(before) {
		t.Fatalf("%s: %d rows after the refusal, want the %d there were before", what, len(after), len(before))
	}
}

func wantPromotions(t *testing.T, what string, mem state.MemoryStore, ids []string) []state.MemoryPromotion {
	t.Helper()
	rows, err := mem.Promotions(context.Background(), ids)
	if err != nil {
		t.Fatalf("%s: Promotions: %v", what, err)
	}
	return rows
}

func wantPromotionCount(t *testing.T, what string, rows []state.MemoryPromotion, want int) {
	t.Helper()
	if len(rows) != want {
		t.Fatalf("%s: %d promotion rows, want %d: %+v", what, len(rows), want, rows)
	}
}

// wantOnePromotion reads the single decision row for one item.
func wantOnePromotion(t *testing.T, what string, mem state.MemoryStore, id string) state.MemoryPromotion {
	t.Helper()
	rows := wantPromotions(t, what, mem, []string{id})
	wantPromotionCount(t, what+", for one item", rows, 1)
	if rows[0].MemoryID != id {
		t.Fatalf("%s: promotion row = %+v, want it for item %s", what, rows[0], id)
	}
	return rows[0]
}

// wantUsage reads usage for ids, or every row for nil.
func wantUsage(t *testing.T, what string, mem state.MemoryStore, ids []string) []state.MemoryUsage {
	t.Helper()
	rows, err := mem.Usage(context.Background(), ids)
	if err != nil {
		t.Fatalf("%s: Usage: %v", what, err)
	}
	return rows
}

func wantUsageCount(t *testing.T, what string, rows []state.MemoryUsage, want int) {
	t.Helper()
	if len(rows) != want {
		t.Fatalf("%s: %d usage rows, want %d: %+v", what, len(rows), want, rows)
	}
}

// wantOneUsage fails unless exactly one instance recorded usage for id.
func wantOneUsage(t *testing.T, what string, mem state.MemoryStore, id string) state.MemoryUsage {
	t.Helper()
	rows := wantUsage(t, what, mem, []string{id})
	wantUsageCount(t, what+", for one item", rows, 1)
	if rows[0].MemoryID != id || rows[0].InstanceID == "" {
		t.Fatalf("%s: usage row = %+v, want it for item %s and attributed to an instance", what, rows[0], id)
	}
	return rows[0]
}

// wantCounts also checks the derived UseCount and Ignored.
func wantCounts(t *testing.T, what string, u state.MemoryUsage, pushes, organic, primed int64) {
	t.Helper()
	uses := organic + primed
	if u.PushCount != pushes || u.OrganicUses != organic || u.PrimedUses != primed ||
		u.UseCount() != uses || u.Ignored() != (pushes > 0 && uses == 0) {
		t.Fatalf("%s: usage = %+v, want (pushes %d, organic %d, primed %d)", what, u, pushes, organic, primed)
	}
}

// wantStamps checks the timestamps apart from the counters, because the two stamps
// exist to keep a push from looking like a read.
func wantStamps(t *testing.T, what string, u state.MemoryUsage, pushed, used bool) {
	t.Helper()
	if u.LastPushedAt.IsZero() == pushed || u.LastUsedAt.IsZero() == used {
		t.Fatalf("%s: usage stamps = (pushed %v, used %v), want them set (%v, %v)",
			what, u.LastPushedAt, u.LastUsedAt, pushed, used)
	}
}

func wantErrIs(t *testing.T, what string, got, want error) {
	t.Helper()
	if !errors.Is(got, want) {
		t.Fatalf("%s = %v, want %v", what, got, want)
	}
}

func mustRecordPush(t *testing.T, mem state.MemoryStore, ids []string) {
	t.Helper()
	if err := mem.RecordPush(context.Background(), ids); err != nil {
		t.Fatalf("RecordPush(%v): %v", ids, err)
	}
}

func mustRecordUse(t *testing.T, mem state.MemoryStore, id string, origin state.UsageOrigin) {
	t.Helper()
	if err := mem.RecordUse(context.Background(), id, origin); err != nil {
		t.Fatalf("RecordUse(%s, %s): %v", id, origin, err)
	}
}

func contents(items []state.MemoryItem) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Content)
	}
	return out
}

// sortedContents compares sets, so a test does not pin the CreatedAt ordering the
// contract leaves open.
func sortedContents(items []state.MemoryItem) []string {
	out := contents(items)
	slices.Sort(out)
	return out
}

func testTombstone(t *testing.T, mem state.MemoryStore) {
	ctx := context.Background()

	a := write(t, mem, state.MemoryItem{Kind: "fact", Content: "ship it"})
	mustDelete(t, mem, a.ID, "the item was just written and is live")
	wantCount(t, "recall after delete",
		recall(t, mem, state.RecallQuery{Query: "ship"}), 0)
	if err := mem.Delete(ctx, a.ID); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("double delete = %v, want ErrNotFound", err)
	}
	if err := mem.Delete(ctx, "missing"); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("Delete(missing) = %v, want ErrNotFound", err)
	}
}

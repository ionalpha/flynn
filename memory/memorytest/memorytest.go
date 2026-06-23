// Package memorytest is the conformance suite for state.MemoryStore. Every backing
// (the in-memory resource store, the SQLite resource store, a host's) runs RunSuite
// and must behave identically, so the typed memory facade is held to one contract no
// matter which resource backend it sits on.
package memorytest

import (
	"context"
	"errors"
	"testing"

	"github.com/ionalpha/flynn/state"
)

// RunSuite runs the full MemoryStore contract against stores built by newStore. Each
// subtest gets a fresh, empty store.
func RunSuite(t *testing.T, newStore func() state.MemoryStore) {
	t.Helper()
	t.Run("WriteRecall", func(t *testing.T) { testWriteRecall(t, newStore()) })
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

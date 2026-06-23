package memory_test

import (
	"context"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/memory"
	"github.com/ionalpha/flynn/memory/memorytest"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/state"
)

// newMemStore builds a memory facade over a fresh in-memory resource store with the
// Memory kind registered.
func newMemStore(t *testing.T) state.MemoryStore {
	t.Helper()
	reg := resource.NewRegistry()
	if err := resource.RegisterCoreKinds(reg); err != nil {
		t.Fatalf("register core kinds: %v", err)
	}
	if err := memory.RegisterKind(reg); err != nil {
		t.Fatalf("register memory kind: %v", err)
	}
	return memory.NewStore(resource.NewMemory(reg))
}

func TestConformance(t *testing.T) {
	memorytest.RunSuite(t, func() state.MemoryStore { return newMemStore(t) })
}

// TestRoundTripProperty asserts that whatever content a memory item carries, a Write
// assigns a fresh id and then Recall returns exactly that content, scope narrows the
// search to it, and a delete by id removes it.
func TestRoundTripProperty(t *testing.T) {
	ctx := context.Background()
	ident := rapid.StringMatching(`[a-z][a-z0-9-]{0,15}`)

	rapid.Check(t, func(rt *rapid.T) {
		store := newMemStore(t)
		kind := rapid.SampledFrom([]string{"fact", "preference", "observation"}).Draw(rt, "kind")
		content := rapid.StringMatching(`[a-zA-Z0-9 ]{1,40}`).Draw(rt, "content")
		source := rapid.String().Draw(rt, "source")
		scope := state.Scope{
			Instance:  ident.Draw(rt, "instance"),
			Project:   ident.Draw(rt, "project"),
			Workspace: ident.Draw(rt, "workspace"),
		}

		written, err := store.Write(ctx, state.MemoryItem{Kind: kind, Content: content, Source: source, Scope: scope})
		if err != nil {
			rt.Fatalf("write: %v", err)
		}
		if written.ID == "" || written.SyncVersion != 1 {
			rt.Fatalf("write envelope = (id %q, sync %d), want id + 1", written.ID, written.SyncVersion)
		}

		// Recalled within its own scope with the exact content round-tripped.
		got, err := store.Recall(ctx, state.RecallQuery{Scope: scope})
		if err != nil {
			rt.Fatalf("recall: %v", err)
		}
		if len(got) != 1 || got[0].ID != written.ID || got[0].Content != content || got[0].Kind != kind || got[0].Source != source {
			rt.Fatalf("recall round-trip mismatch: %+v", got)
		}

		// A delete by id removes it from recall.
		if err := store.Delete(ctx, written.ID); err != nil {
			rt.Fatalf("delete: %v", err)
		}
		if after, _ := store.Recall(ctx, state.RecallQuery{Scope: scope}); len(after) != 0 {
			rt.Fatalf("recall after delete = %d, want 0", len(after))
		}
	})
}

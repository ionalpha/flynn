package memory_test

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/memory"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/state"
)

// The conformance suite can only assert an item comfortably dead or comfortably
// alive, because it runs against whatever clock the backend holds. Driving the
// facade's own clock is what pins the transition: the same item, unchanged in the
// store, is recalled before its expiry and not after.
func TestRecallDropsItemsAsTheClockPasses(t *testing.T) {
	ctx := context.Background()
	start := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	clk := clock.NewManual(start)
	store := memory.NewStore(backing(t), memory.WithClock(clk))

	expires := start.Add(time.Hour)
	if _, err := store.Write(ctx, state.MemoryItem{
		Kind: "fact", Content: "the token rotates at one", ExpiresAt: expires,
	}); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := store.Recall(ctx, state.RecallQuery{})
	if err != nil {
		t.Fatalf("recall before expiry: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("recall before expiry returned %d items, want 1", len(got))
	}
	if !got[0].ExpiresAt.Equal(expires) {
		t.Errorf("recalled ExpiresAt = %v, want %v", got[0].ExpiresAt, expires)
	}

	// Up to the instant itself the item is still live; at it, it is gone. Expiry is
	// half-open, matching the Since/Until window.
	clk.Set(expires.Add(-time.Nanosecond))
	if got, err = store.Recall(ctx, state.RecallQuery{}); err != nil || len(got) != 1 {
		t.Fatalf("recall a nanosecond before expiry = %d items, %v; want 1", len(got), err)
	}
	clk.Set(expires)
	if got, err = store.Recall(ctx, state.RecallQuery{}); err != nil || len(got) != 0 {
		t.Fatalf("recall at the expiry instant = %d items, %v; want none", len(got), err)
	}

	// A nil clock option is ignored rather than installing one that panics on the
	// next read.
	if s := memory.NewStore(backing(t), memory.WithClock(nil)); s == nil {
		t.Fatal("WithClock(nil) must leave the store usable")
	}
}

// Memory resources written before provenance became a list are already stored, and
// reading one back has to keep the source it recorded rather than reporting an item
// with no provenance at all.
func TestRecallReadsLegacySingleSourceResources(t *testing.T) {
	ctx := context.Background()
	rs := backing(t)
	legacy := resource.Resource{
		APIVersion: memory.GroupVersion,
		Kind:       memory.Kind,
		Name:       "mem-legacy",
		Spec:       json.RawMessage(`{"kind":"fact","content":"written under the old shape","source":"chat"}`),
	}
	if _, err := rs.Put(ctx, legacy); err != nil {
		t.Fatalf("seed a legacy resource: %v", err)
	}

	got, err := memory.NewStore(rs).Recall(ctx, state.RecallQuery{Query: "old shape"})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("recall returned %d items, want 1", len(got))
	}
	if !slices.Equal(got[0].Sources, []string{"chat"}) {
		t.Errorf("legacy item read back with Sources %v, want [chat]", got[0].Sources)
	}
	// It never had an expiry, and reading it must not invent one.
	if !got[0].ExpiresAt.IsZero() {
		t.Errorf("legacy item read back with ExpiresAt %v, want none", got[0].ExpiresAt)
	}
}

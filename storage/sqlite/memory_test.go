package sqlite_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/memory"
	"github.com/ionalpha/flynn/memory/memorytest"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/state"
	"github.com/ionalpha/flynn/storage/sqlite"
)

// TestMemoryFacadeConformance proves the typed memory facade behaves identically over
// the durable SQLite resource backend as over the in-memory one: the same MemoryStore
// contract, now persisted, schema-admitted, and event-sourced on the shared spine.
func TestMemoryFacadeConformance(t *testing.T) {
	memorytest.RunSuite(t, func() state.MemoryStore {
		reg := resource.NewRegistry()
		if err := resource.RegisterCoreKinds(reg); err != nil {
			t.Fatalf("register core kinds: %v", err)
		}
		if err := memory.RegisterKind(reg); err != nil {
			t.Fatalf("register memory kind: %v", err)
		}
		p, err := sqlite.Open(context.Background(), ":memory:")
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		return memory.NewStore(p.Resources(reg))
	})
}

// TestProviderMemoryConformance runs the same contract against the provider's own
// memory store - the FTS-backed one behind state.Provider, distinct from the
// facade above. It is a separate implementation of MemoryStore with its own SQL,
// and it was not covered by this suite: statetest exercises the provider's CRUD
// but never a recall that resolves across scope levels, so the SQL that widens one
// had nothing holding it to the shared contract.
func TestProviderMemoryConformance(t *testing.T) {
	memorytest.RunSuite(t, func() state.MemoryStore {
		p, err := sqlite.Open(context.Background(), ":memory:")
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		t.Cleanup(func() { _ = p.Close() })
		return p.Memory()
	})
}

// The provider's memory store answers expiry in SQL, in two separate query shapes:
// a prepared statement for the scoped, query-less read an agent issues at startup,
// and a query built per call for everything else. The conformance suite proves both
// shapes drop an item an hour dead, but only a driven clock can prove the predicate
// is exact rather than approximately right, and that the two shapes agree on the
// same instant.
func TestProviderMemoryExpiryIsExactInBothQueryShapes(t *testing.T) {
	ctx := context.Background()
	start := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	clk := clock.NewManual(start)
	p, err := sqlite.Open(ctx, ":memory:", sqlite.WithClock(clk))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	mem := p.Memory()
	scope := state.Scope{Project: "p"}
	expires := start.Add(time.Hour)
	if _, err := mem.Write(ctx, state.MemoryItem{
		Kind: "fact", Content: "the token rotates at one", Scope: scope, ExpiresAt: expires,
	}); err != nil {
		t.Fatalf("write: %v", err)
	}

	// The prepared shape: one scope, no query, no selectors.
	fastPath := state.RecallQuery{Scope: scope}
	// The built shape: a full-text match, which is a different statement entirely.
	builtPath := state.RecallQuery{Query: "token"}

	count := func(q state.RecallQuery) int {
		t.Helper()
		got, err := mem.Recall(ctx, q)
		if err != nil {
			t.Fatalf("recall %+v: %v", q, err)
		}
		return len(got)
	}

	for _, c := range []struct {
		what string
		now  time.Time
		want int
	}{
		{"before expiry", start, 1},
		{"a nanosecond before expiry", expires.Add(-time.Nanosecond), 1},
		{"at the expiry instant, which is inclusive", expires, 0},
		{"after expiry", expires.Add(time.Hour), 0},
	} {
		clk.Set(c.now)
		if got := count(fastPath); got != c.want {
			t.Errorf("scoped query-less recall %s = %d items, want %d", c.what, got, c.want)
		}
		if got := count(builtPath); got != c.want {
			t.Errorf("full-text recall %s = %d items, want %d", c.what, got, c.want)
		}
	}
}

// Provenance survives the round trip through the durable store, including the
// characters a column packing several sources into one string would break on.
func TestProviderMemoryStoresEverySource(t *testing.T) {
	ctx := context.Background()
	p, err := sqlite.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	sources := []string{"user:operator", `a "quoted" one`, "a,comma"}
	mem := p.Memory()
	if _, err := mem.Write(ctx, state.MemoryItem{
		Kind: "fact", Content: "three inputs", Sources: sources,
	}); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := mem.Recall(ctx, state.RecallQuery{Query: "inputs"})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("recall returned %d items, want 1", len(got))
	}
	if !slices.Equal(got[0].Sources, sources) {
		t.Errorf("recalled Sources %v, want %v", got[0].Sources, sources)
	}
}

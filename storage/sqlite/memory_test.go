package sqlite_test

import (
	"context"
	"testing"

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

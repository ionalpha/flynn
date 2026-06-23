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

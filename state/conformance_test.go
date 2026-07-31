package state_test

import (
	"testing"

	"github.com/ionalpha/flynn/memory/memorytest"
	"github.com/ionalpha/flynn/state"
	"github.com/ionalpha/flynn/state/statetest"
)

// TestMemoryConformance runs the shared state.Provider contract against the
// in-memory provider. The SQLite (and later Postgres) providers run the same
// suite, so all backends are held to byte-identical behaviour.
func TestMemoryConformance(t *testing.T) {
	statetest.RunSuite(t, func() state.Provider { return state.NewMemory() })
}

// TestMemoryStoreConformance holds this provider's MemoryStore to the same
// MemoryStore contract the resource-backed and SQLite stores run. It is a third
// implementation of that interface and was the only one the suite did not cover,
// which is how it could resolve scopes differently from the others without any
// test noticing.
func TestMemoryStoreConformance(t *testing.T) {
	memorytest.RunSuite(t, func() state.MemoryStore { return state.NewMemory().Memory() })
}

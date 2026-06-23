package state_test

import (
	"testing"

	"github.com/ionalpha/flynn/state"
	"github.com/ionalpha/flynn/state/statetest"
)

// TestMemoryConformance runs the shared state.Provider contract against the
// in-memory provider. The SQLite (and later Postgres) providers run the same
// suite, so all backends are held to byte-identical behaviour.
func TestMemoryConformance(t *testing.T) {
	statetest.RunSuite(t, func() state.Provider { return state.NewMemory() })
}

package state_test

import (
	"testing"

	"github.com/ionalpha/flynn/state"
	"github.com/ionalpha/flynn/state/statetest"
)

// TestMemoryPackReconcile holds the in-memory skill store to the pack-update
// contract the SQLite store and a host's own store run.
func TestMemoryPackReconcile(t *testing.T) {
	statetest.RunPackReconcileSuite(t, func() state.SkillStore { return state.NewMemory().Skills() })
}

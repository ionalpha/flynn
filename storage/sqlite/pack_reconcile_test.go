package sqlite_test

import (
	"context"
	"testing"

	"github.com/ionalpha/flynn/state"
	"github.com/ionalpha/flynn/state/statetest"
	"github.com/ionalpha/flynn/storage/sqlite"
)

// TestSQLitePackReconcile runs the pack-update contract against the SQLite skill
// store, which has to persist the record of what the pack last wrote.
func TestSQLitePackReconcile(t *testing.T) {
	statetest.RunPackReconcileSuite(t, func() state.SkillStore {
		p, err := sqlite.Open(context.Background(), ":memory:")
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		t.Cleanup(func() { _ = p.Close() })
		return p.Skills()
	})
}

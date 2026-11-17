package sqlite_test

import (
	"context"
	"testing"

	"github.com/ionalpha/flynn/state"
	"github.com/ionalpha/flynn/state/statetest"
	"github.com/ionalpha/flynn/storage/sqlite"
)

// TestModelUsageStoreConformance holds the SQLite usage store to the contract the
// in-memory double passes, queries and ordering included.
func TestModelUsageStoreConformance(t *testing.T) {
	statetest.RunModelUsageSuite(t, func() state.ModelUsageStore {
		p, err := sqlite.Open(context.Background(), ":memory:")
		if err != nil {
			panic(err)
		}
		t.Cleanup(func() { _ = p.Close() })
		return p.ModelUsage()
	})
}

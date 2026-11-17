package statetest_test

import (
	"testing"

	"github.com/ionalpha/flynn/state"
	"github.com/ionalpha/flynn/state/statetest"
)

// TestMemoryUsageStoreConformance holds the in-memory double to the contract the
// durable stores are held to, so a test using it as a stand-in is testing against
// the same behaviour.
func TestMemoryUsageStoreConformance(t *testing.T) {
	statetest.RunUsageStoreSuite(t, func() state.UsageStore { return statetest.NewMemoryUsageStore() })
}

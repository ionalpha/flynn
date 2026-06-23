package resource_test

import (
	"testing"

	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/resource/resourcetest"
)

// TestMemoryConformance holds the in-memory Store to the full substrate contract.
// The SQLite (and any host) backend runs the identical suite, so they stay
// byte-for-byte interchangeable.
func TestMemoryConformance(t *testing.T) {
	resourcetest.RunSuite(t, func(reg *resource.Registry) resource.Store {
		return resource.NewMemory(reg)
	})
}

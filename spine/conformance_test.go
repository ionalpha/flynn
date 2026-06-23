package spine_test

import (
	"testing"

	"github.com/ionalpha/flynn/spine"
	"github.com/ionalpha/flynn/spine/spinetest"
)

// TestMemoryLogConformance runs the shared spine.Log contract against the
// in-memory log. The SQLite log runs the same suite, so both backends are held
// to byte-for-byte identical ordering and immutability behaviour.
func TestMemoryLogConformance(t *testing.T) {
	spinetest.RunSuite(t, func() spine.Log { return spine.NewMemoryLog() })
}

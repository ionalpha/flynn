package resourcetest_test

import (
	"testing"

	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/resource/resourcetest"
)

// TestSuiteHoldsTheReferenceBackend runs the conformance suite against the
// reference in-memory Store. It is the suite's own gate: a contract test that
// passes vacuously (a helper that never asserts, an assertion that cannot fail) is
// worthless to the durable backends that depend on it, so the suite must be
// exercised end to end in its own package too.
func TestSuiteHoldsTheReferenceBackend(t *testing.T) {
	resourcetest.RunSuite(t, func(reg *resource.Registry) resource.Store {
		return resource.NewMemory(reg)
	})
}

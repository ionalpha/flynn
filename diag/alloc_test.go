//go:build !race

// Allocation ceilings for the disabled path. A binary built with profiling support
// must not pay for it when nobody asked: Start with an empty Config allocates
// nothing and starts nothing, and every method on the nil bundle it returns is
// free. Excluded under -race (instrumentation skews allocation counts); dev/bench
// and the CI bench job run it.

package diag

import "testing"

func TestAllocCeilingDisabledStartStop(t *testing.T) {
	allocs := testing.AllocsPerRun(100, func() {
		b, err := Start(Config{})
		if err != nil || b != nil {
			t.Fatalf("Start(Config{}) = %v, %v; want nil, nil", b, err)
		}
		_ = b.Stop()
	})
	if allocs != 0 {
		t.Errorf("a disabled Start/Stop allocates %.0f/op, want 0: profiling must be free when off", allocs)
	}
}

func TestAllocCeilingNilBundleMethods(t *testing.T) {
	var b *Bundle
	allocs := testing.AllocsPerRun(100, func() {
		b.Annotate("run", "r1")
		_ = b.Dir()
		_ = b.ID()
	})
	if allocs != 0 {
		t.Errorf("nil-bundle methods allocate %.0f/op, want 0: a caller holds a bundle unconditionally", allocs)
	}
}

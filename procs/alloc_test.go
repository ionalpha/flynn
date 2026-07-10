//go:build !race

// Allocation ceiling for the sampler's read. Live is called once per timeline sample for
// the life of a profiled process, including on a 24/7 host, so it must be an atomic load
// and nothing else: no allocation, and in particular no walk of the machine's process
// table. Excluded under -race (instrumentation skews allocation counts); dev/bench and
// the CI bench job run it.

package procs

import "testing"

func TestLiveAllocatesNothing(t *testing.T) {
	var r Registry
	reaped := r.Started()
	defer reaped()

	if got := testing.AllocsPerRun(100, func() { sink = r.Live() }); got != 0 {
		t.Errorf("Registry.Live allocates %v times per call, want 0", got)
	}
	if got := testing.AllocsPerRun(100, func() { sink = Live() }); got != 0 {
		t.Errorf("Live allocates %v times per call, want 0", got)
	}
}

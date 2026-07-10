package procs

import (
	"strconv"
	"testing"
)

// BenchmarkLive is the read the timeline sampler performs once per interval. It is the
// benchmark the counter exists to make possible: the implementation it replaced opened
// and read one file per process on the machine on every one of these calls.
func BenchmarkLive(b *testing.B) {
	var r Registry
	reaped := r.Started()
	defer reaped()

	b.ReportAllocs()
	for b.Loop() {
		sink = r.Live()
	}
}

// BenchmarkLiveByLiveChildren shows the read is flat in the number of children the
// process is holding: every case is one atomic load, so the ns/op of the 1 case and the
// 4096 case are the same to within noise. Nothing here scales with the machine's process
// table either, which is the property the OS scan could not offer at any size.
func BenchmarkLiveByLiveChildren(b *testing.B) {
	for _, n := range []int{1, 16, 256, 4096} {
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			var r Registry
			reaps := make([]func(), 0, n)
			for range n {
				reaps = append(reaps, r.Started())
			}
			defer func() {
				for _, reap := range reaps {
					reap()
				}
			}()
			if r.Live() != n {
				b.Fatalf("setup: %d live, want %d", r.Live(), n)
			}

			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				sink = r.Live()
			}
		})
	}
}

// BenchmarkStartedReaped is the cost the spawners pay per child: one increment, one
// closure, one decrement. It is charged once per process launched, against an
// exec that costs milliseconds.
func BenchmarkStartedReaped(b *testing.B) {
	var r Registry

	b.ReportAllocs()
	for b.Loop() {
		r.Started()()
	}
}

// sink keeps the benchmarked loads from being optimized away.
var sink int

package chain

import (
	"testing"
)

// The canonical-codec benchmarks: CanonicalBytes is on every chained append and
// every verification, so its cost (and the allocation ceiling in alloc_test.go)
// is pinned here. Run via dev/bench; the CI bench job smokes them.

func BenchmarkCanonicalBytes(b *testing.B) {
	e := sampleEvent()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := CanonicalBytes(e); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDecodeCanonical(b *testing.B) {
	e := sampleEvent()
	bs, err := CanonicalBytes(e)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := DecodeCanonical(bs); err != nil {
			b.Fatal(err)
		}
	}
}

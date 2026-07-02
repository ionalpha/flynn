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

// BenchmarkEventProof extracts a single-event proof from a sealed 1024-event
// run: pure node-map lookup plus path rehash since the seal-time snapshot.
func BenchmarkEventProof(b *testing.B) {
	priv, _ := testKey(0x10)
	signer, err := NewEd25519RootSigner("inst", priv)
	if err != nil {
		b.Fatal(err)
	}
	bld := NewBuilder("flynn://run/bench")
	for i := range 1024 {
		e := sampleEvent()
		e.Seq = int64(i + 1)
		if err := bld.Add(e); err != nil {
			b.Fatal(err)
		}
	}
	sealed, err := bld.Seal(signer)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := sealed.EventProof(512); err != nil {
			b.Fatal(err)
		}
	}
}

package resource_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/ionalpha/flynn/resource"
)

// The write-path benchmarks. Hash/SpecHash/Hashes are the canonicalization core
// every write pays; MemoryPut is the full command path (admission, stamp, hash,
// encode, append, project). Run via dev/bench; the CI bench job smokes them and
// the !race allocation ceilings in alloc_test.go pin their allocation cost.

const benchAPIVersion = "bench.ionagent.io/v1"

var benchSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "size": {"type": "string"},
    "replicas": {"type": "integer"}
  }
}`)

// benchRegistry builds a registry with the bench kind, panicking on error so it
// is usable from benchmarks (which have no *testing.T).
func benchRegistry() *resource.Registry {
	reg := resource.NewRegistry()
	if err := resource.RegisterCoreKinds(reg); err != nil {
		panic(err)
	}
	if err := reg.Register(resource.Kind{APIVersion: benchAPIVersion, Name: "Gadget", Schema: benchSchema}); err != nil {
		panic(err)
	}
	return reg
}

// benchResource is a representative record: a few labels and annotations, a
// small structured spec, and a status a controller would write.
func benchResource(name string) resource.Resource {
	return resource.Resource{
		APIVersion:  benchAPIVersion,
		Kind:        "Gadget",
		Name:        name,
		Labels:      map[string]string{"tier": "pro", "app": "bench"},
		Annotations: map[string]string{"note": "benchmark fixture"},
		Spec:        json.RawMessage(`{"size":"m","replicas":3}`),
		Status:      json.RawMessage(`{"phase":"Ready","observedSpecHash":"abc"}`),
	}
}

func BenchmarkHash(b *testing.B) {
	r := benchResource("bench")
	b.ReportAllocs()
	for b.Loop() {
		if _, err := resource.Hash(r); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSpecHash(b *testing.B) {
	r := benchResource("bench")
	b.ReportAllocs()
	for b.Loop() {
		if _, err := resource.SpecHash(r); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkHashes(b *testing.B) {
	r := benchResource("bench")
	b.ReportAllocs()
	for b.Loop() {
		if _, _, err := resource.Hashes(r); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkMemoryPut measures one full write through the in-memory command path:
// admission, stamping (both hashes), the single payload Marshal, the log append,
// and the raw-payload projection.
func BenchmarkMemoryPut(b *testing.B) {
	ctx := context.Background()
	store := resource.NewMemory(benchRegistry())
	defer func() { _ = store.Close() }()
	r := benchResource("bench")
	b.ReportAllocs()
	for b.Loop() {
		if _, err := store.Put(ctx, r); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkMemoryList pins the read side at a fixed population so the write-path
// numbers above have a reference point.
func BenchmarkMemoryList(b *testing.B) {
	ctx := context.Background()
	store := resource.NewMemory(benchRegistry())
	defer func() { _ = store.Close() }()
	for i := range 100 {
		if _, err := store.Put(ctx, benchResource(fmt.Sprintf("bench-%03d", i))); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := store.List(ctx, "Gadget", resource.Scope{}, nil); err != nil {
			b.Fatal(err)
		}
	}
}

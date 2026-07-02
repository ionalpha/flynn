package sqlite_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/state"
	"github.com/ionalpha/flynn/storage/sqlite"
)

// The durable write-path benchmarks: one resource Put and one turn append,
// end to end through the command path (tx, CAS lookup, stamp, raw-payload
// append, record projection) against a ":memory:" database. Run via dev/bench;
// the CI bench job smokes them.

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

func BenchmarkResourcePut(b *testing.B) {
	ctx := context.Background()
	p, err := sqlite.Open(ctx, ":memory:")
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	store := p.Resources(benchRegistry())
	r := resource.Resource{
		APIVersion:  benchAPIVersion,
		Kind:        "Gadget",
		Name:        "bench",
		Labels:      map[string]string{"tier": "pro", "app": "bench"},
		Annotations: map[string]string{"note": "benchmark fixture"},
		Spec:        json.RawMessage(`{"size":"m","replicas":3}`),
		Status:      json.RawMessage(`{"phase":"Ready","observedSpecHash":"abc"}`),
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := store.Put(ctx, r); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAppendTurn(b *testing.B) {
	ctx := context.Background()
	p, err := sqlite.Open(ctx, ":memory:")
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	ses, err := p.Sessions().Create(ctx, state.Session{Title: "bench", Model: "m"})
	if err != nil {
		b.Fatal(err)
	}
	i := 0
	b.ReportAllocs()
	for b.Loop() {
		i++
		_, err := p.Sessions().AppendTurn(ctx, state.Turn{
			SessionID: ses.ID,
			Role:      "user",
			Content:   fmt.Sprintf("turn %d: a short representative message body", i),
		})
		if err != nil {
			b.Fatal(err)
		}
	}
}

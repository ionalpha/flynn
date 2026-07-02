package sqlite_test

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/spine"
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

// BenchmarkEventAppend measures the spine append: one transaction around the
// folded assign-seq-and-insert statement (prepared at Open). The ns/op and
// allocs/op are the gate on the event write path.
func BenchmarkEventAppend(b *testing.B) {
	ctx := context.Background()
	p, err := sqlite.Open(ctx, ":memory:")
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	log := p.Log()
	in := spine.AppendInput{
		Stream:  "bench-stream",
		Type:    "bench.event",
		Actor:   spine.ActorAgent,
		Payload: map[string]any{"key": "value", "n": 1},
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := log.Append(ctx, in); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkResourceGet measures the point read on the control loop's hot path
// against a populated table.
func BenchmarkResourceGet(b *testing.B) {
	ctx := context.Background()
	p, err := sqlite.Open(ctx, ":memory:")
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	store := p.Resources(benchRegistry())
	for i := range 100 {
		r := resource.Resource{
			APIVersion: benchAPIVersion, Kind: "Gadget", Name: fmt.Sprintf("bench-%d", i),
			Spec: json.RawMessage(`{"size":"m","replicas":3}`),
		}
		if _, err := store.Put(ctx, r); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := store.Get(ctx, "Gadget", resource.Scope{}, "bench-50"); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkConcurrentReadsUnderWrites is the gate on the read pool: parallel
// point reads against a FILE database while one goroutine writes continuously.
// On a single shared connection every read queues behind the writer's
// transactions; on the pool, WAL serves the readers a committed view
// concurrently. Regression shows up as ns/op collapsing toward the write rate.
func BenchmarkConcurrentReadsUnderWrites(b *testing.B) {
	ctx := context.Background()
	p, err := sqlite.Open(ctx, filepath.Join(b.TempDir(), "bench.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	store := p.Resources(benchRegistry())
	for i := range 100 {
		r := resource.Resource{
			APIVersion: benchAPIVersion, Kind: "Gadget", Name: fmt.Sprintf("bench-%d", i),
			Spec: json.RawMessage(`{"size":"m","replicas":3}`),
		}
		if _, err := store.Put(ctx, r); err != nil {
			b.Fatal(err)
		}
	}

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		r := resource.Resource{
			APIVersion: benchAPIVersion, Kind: "Gadget", Name: "bench-writer",
			Spec: json.RawMessage(`{"size":"m","replicas":3}`),
		}
		for {
			select {
			case <-stop:
				return
			default:
				if _, err := store.Put(ctx, r); err != nil {
					return
				}
			}
		}
	}()

	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := store.Get(ctx, "Gadget", resource.Scope{}, "bench-50"); err != nil {
				b.Fatal(err)
			}
		}
	})
	close(stop)
	<-done
}

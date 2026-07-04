package chain

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/spine"
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

// shardedLog is a benchmark-only spine.Log whose per-stream sequencing uses a
// sync.Map of atomic counters, so it adds no global serialization point of its own.
// This isolates what BenchmarkRecordingLogParallelStreams measures to the recording
// layer: with the per-recorder lock, appends across distinct streams scale with GOMAXPROCS;
// a single global lock over encode+hash would flatten them regardless of the inner log.
type shardedLog struct {
	*spine.MemoryLog
	clk  clock.Clock
	seqs sync.Map // stream -> *atomic.Int64
}

func (l *shardedLog) Append(_ context.Context, in spine.AppendInput) (spine.Event, error) {
	ctr, _ := l.seqs.LoadOrStore(in.Stream, new(atomic.Int64))
	seq := ctr.(*atomic.Int64).Add(1)
	e, _, err := in.Materialize(l.clk, seq)
	return e, err
}

// BenchmarkRecordingLogParallelStreams appends concurrently, one distinct stream per
// goroutine. Each stream's encode+hash runs under its own recorder lock, so the global
// lock is held only for the map lookup. Compare -cpu=1 against -cpu=8 to see fan-out scale.
func BenchmarkRecordingLogParallelStreams(b *testing.B) {
	rl := NewRecordingLog(&shardedLog{MemoryLog: spine.NewMemoryLog(), clk: clock.System{}}, nil)
	ctx := context.Background()
	var next atomic.Int64
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		stream := "run/" + strconv.FormatInt(next.Add(1), 10)
		for pb.Next() {
			if _, err := rl.Append(ctx, spine.AppendInput{
				Stream:  stream,
				Type:    "action.dispatched",
				Actor:   spine.ActorAgent,
				Payload: map[string]any{"i": int64(1)},
			}); err != nil {
				b.Fatal(err)
			}
		}
	})
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

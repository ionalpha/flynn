package resource_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/ids"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/spine"
)

// cyclicReader is a deterministic, unbounded entropy source: a replay re-seeds it
// identically, so the ids.Generator yields the same IDs on every run. The default
// crypto/rand source would make two runs diverge, which is exactly the kind of
// nondeterminism this test exists to catch.
type cyclicReader struct {
	seed []byte
	i    int
}

func (r *cyclicReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = r.seed[r.i%len(r.seed)]
		r.i++
	}
	return len(p), nil
}

// TestReplayEquivalence is the determinism guard for the event spine. The same
// scenario, run twice under the same seeded clock and entropy, must produce a
// byte-identical event stream. It catches the nondeterminism the linter cannot:
// an ID that does not reseed, a wall-clock read reached through a dependency, or
// map-iteration order leaking into serialized output. Any such source makes the
// two runs diverge, failing the byte comparison.
//
// It runs at the resource store, the event-sourcing core, because that is where
// IDs, the clock, payloads, and ordering all meet on one stream.
func TestReplayEquivalence(t *testing.T) {
	run := func() []spine.Event {
		ctx := context.Background()
		clk := clock.NewManual(time.Unix(1_700_000_000, 0))
		gen := ids.NewGenerator(
			ids.WithClock(clk),
			ids.WithEntropy(&cyclicReader{seed: []byte{1, 2, 3, 5, 7, 11, 13, 17}}),
		)
		log := spine.NewMemoryLog(spine.WithClock(clk))

		reg := resource.NewRegistry()
		if err := reg.Register(resource.Kind{APIVersion: "test/v1", Name: "Thing"}); err != nil {
			t.Fatalf("register kind: %v", err)
		}
		store := resource.NewMemory(reg,
			resource.WithClock(clk),
			resource.WithIDGenerator(gen),
			resource.WithEventLog(log),
			resource.WithInstanceID("node-1"),
		)

		// Creates with server-assigned IDs (exercising the id generator), plus a
		// named resource created then updated (a second write to the same record).
		for _, name := range []string{"alpha", "beta", "gamma"} {
			if _, err := store.Put(ctx, resource.Resource{
				APIVersion: "test/v1", Kind: "Thing", GenerateName: name + "-",
				Spec: json.RawMessage(`{"phase":"new","label":"` + name + `"}`),
			}); err != nil {
				t.Fatalf("create %s: %v", name, err)
			}
		}
		for _, phase := range []string{"new", "ready"} {
			if _, err := store.Put(ctx, resource.Resource{
				APIVersion: "test/v1", Kind: "Thing", Name: "fixed",
				Spec: json.RawMessage(`{"phase":"` + phase + `"}`),
			}); err != nil {
				t.Fatalf("write fixed (%s): %v", phase, err)
			}
		}

		events, err := log.Read(ctx, spine.Query{Stream: resource.ResourceStream})
		if err != nil {
			t.Fatalf("read spine: %v", err)
		}
		return events
	}

	first, second := run(), run()

	a, err := json.Marshal(first)
	if err != nil {
		t.Fatalf("marshal first: %v", err)
	}
	b, err := json.Marshal(second)
	if err != nil {
		t.Fatalf("marshal second: %v", err)
	}
	if string(a) != string(b) {
		t.Fatalf("replay diverged (a nondeterminism source leaked into the spine):\n first:  %s\n second: %s", a, b)
	}

	// Guard against a vacuous pass: the scenario must actually produce events with
	// generated IDs, so the comparison is exercising the seams, not empty logs.
	if len(first) == 0 {
		t.Fatal("no events on the spine; the scenario did not exercise the determinism seams")
	}
}

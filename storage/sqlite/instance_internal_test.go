package sqlite

// The two identity options, together. The store reports the instance id it stamps onto
// records, so a running agent can address its own Instance resource, and an injected
// generator is where record ids come from. Two stores wired to the same clock and the
// same entropy mint the same id for the same write, which is what makes replay
// deterministic.

import (
	"context"
	"testing"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/ids"
	"github.com/ionalpha/flynn/state"
)

// constReader is a deterministic entropy source: an id generator built on it and a manual
// clock produces the same ids on every run, which is what makes replay reproducible.
type constReader struct{ b byte }

func (r constReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = r.b
	}
	return len(p), nil
}

// TestInstanceIDAndInjectedIDGenerator covers the two identity options together: the store
// reports the instance id it stamps onto records (so a running agent can address its own
// Instance resource), and an injected generator is the one the records get their ids from.
// Two stores wired with the same clock and the same entropy therefore mint the same id for
// the same write, which is the basis of deterministic replay.
func TestInstanceIDAndInjectedIDGenerator(t *testing.T) {
	ctx := context.Background()
	newSeeded := func() *Store {
		clk := clock.NewManual(testAt)
		gen := ids.NewGenerator(ids.WithClock(clk), ids.WithEntropy(constReader{b: 0x5a}))
		s, err := Open(ctx, ":memory:", WithClock(clk), WithInstanceID("node-9"), WithIDGenerator(gen))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	}

	a, b := newSeeded(), newSeeded()
	if got := a.InstanceID(); got != "node-9" {
		t.Fatalf("InstanceID() = %q, want node-9", got)
	}
	ska, err := a.Skills().Upsert(ctx, state.Skill{Slug: "deploy", Body: "ship"})
	if err != nil {
		t.Fatal(err)
	}
	skb, err := b.Skills().Upsert(ctx, state.Skill{Slug: "deploy", Body: "ship"})
	if err != nil {
		t.Fatal(err)
	}
	if ska.ID == "" || ska.ID != skb.ID {
		t.Fatalf("seeded stores minted %q and %q, want one deterministic id", ska.ID, skb.ID)
	}
	if ska.OriginInstanceID != "node-9" {
		t.Fatalf("origin instance = %q, want node-9", ska.OriginInstanceID)
	}
}

package resource

import (
	"testing"

	"github.com/ionalpha/flynn/hlc"
	"github.com/ionalpha/flynn/spine"
)

// foldResolve reduces a set of replicated writes to a single winner by applying
// Resolve pairwise, the way Merge accumulates state as records arrive. seed is the
// starting record (the first to arrive).
func foldResolve(seed Resource, rest []Resource) Resource {
	cur := seed
	for _, r := range rest {
		if w, take := Resolve(r, cur); take {
			cur = w
		}
	}
	return cur
}

// permute yields every ordering of rs (n is small in these tests).
func permute(rs []Resource) [][]Resource {
	if len(rs) <= 1 {
		return [][]Resource{append([]Resource(nil), rs...)}
	}
	var out [][]Resource
	for i := range rs {
		rest := make([]Resource, 0, len(rs)-1)
		rest = append(rest, rs[:i]...)
		rest = append(rest, rs[i+1:]...)
		for _, p := range permute(rest) {
			out = append(out, append([]Resource{rs[i]}, p...))
		}
	}
	return out
}

// TestResolveConverges is the core convergence property: because Resolve picks the
// maximum of a total order over (provenance rank, HLC, writer id), folding a set of
// conflicting writes yields the same winner no matter the order they arrive. This
// is what lets independent instances reach identical state from out-of-order sync.
func TestResolveConverges(t *testing.T) {
	w := func(wall int64, ctr uint16, writer string, actor spine.ActorType, deleted bool) Resource {
		return Resource{
			ID: "same", APIVersion: "t/v1", Kind: "T", Name: "n",
			Envelope: Envelope{
				OriginInstanceID: writer, LastWriterID: writer, WriterActor: actor,
				UpdatedHLC: hlc.Time{Wall: wall, Counter: ctr}, Deleted: deleted,
			},
		}
	}
	sets := [][]Resource{
		{ // plain LWW by HLC
			w(100, 0, "a", spine.ActorAgent, false),
			w(300, 0, "b", spine.ActorAgent, false),
			w(200, 0, "c", spine.ActorAgent, false),
		},
		{ // human precedence beats a strictly newer agent write
			w(100, 0, "a", spine.ActorHuman, false),
			w(900, 0, "b", spine.ActorAgent, false),
			w(950, 5, "c", spine.ActorSystem, false),
		},
		{ // equal HLC resolves by writer id; a tombstone is just another write
			w(500, 1, "a", spine.ActorAgent, false),
			w(500, 1, "z", spine.ActorAgent, true),
			w(500, 1, "m", spine.ActorAgent, false),
		},
		{ // tombstone with the highest HLC wins (delete propagates)
			w(100, 0, "a", spine.ActorAgent, false),
			w(400, 0, "b", spine.ActorAgent, true),
			w(300, 0, "c", spine.ActorAgent, false),
		},
	}

	for i, set := range sets {
		var want Resource
		for j, order := range permute(set) {
			got := foldResolve(order[0], order[1:])
			if j == 0 {
				want = got
				continue
			}
			if got.LastWriterID != want.LastWriterID || got.UpdatedHLC != want.UpdatedHLC || got.Deleted != want.Deleted {
				t.Fatalf("set %d not convergent: order %d picked (%s,%v,del=%v), want (%s,%v,del=%v)",
					i, j, got.LastWriterID, got.UpdatedHLC, got.Deleted, want.LastWriterID, want.UpdatedHLC, want.Deleted)
			}
		}
	}
}

// TestResolveIdempotent asserts re-applying the winning record never flips the
// decision: once a record has won, seeing it again is a no-op.
func TestResolveIdempotent(t *testing.T) {
	a := Resource{ID: "x", Envelope: Envelope{LastWriterID: "a", WriterActor: spine.ActorAgent, UpdatedHLC: hlc.Time{Wall: 100}}}
	b := Resource{ID: "x", Envelope: Envelope{LastWriterID: "b", WriterActor: spine.ActorAgent, UpdatedHLC: hlc.Time{Wall: 200}}}
	winner, take := Resolve(b, a)
	if !take || winner.LastWriterID != "b" {
		t.Fatalf("Resolve(b,a) = (%q,%v), want b taken", winner.LastWriterID, take)
	}
	if _, take := Resolve(winner, winner); take {
		t.Fatal("Resolve of a record against itself must not take (idempotent)")
	}
	if _, take := Resolve(a, winner); take {
		t.Fatal("re-applying the losing record must not flip the winner")
	}
}

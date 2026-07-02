package envelope

import (
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/hlc"
)

// TestPropEnvelopeInvariants drives a random op sequence (create, update, bump,
// tombstone) against one envelope and checks the invariants fleet merge rests
// on: SyncVersion counts every write exactly once (starting at 1), the origin
// set at creation never changes, the last writer is always the most recent
// writer, and Deleted is true exactly when the last mutation was a tombstone.
func TestPropEnvelopeInvariants(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		writers := rapid.SampledFrom([]string{"w0", "w1", "w2"})
		var e Envelope
		creator := writers.Draw(t, "creator")
		StampCreate(&e, creator, hlc.Time{Wall: 1})
		origin := e.OriginInstanceID

		writes := int64(1)
		lastWriter := creator
		deleted := false
		steps := rapid.IntRange(0, 20).Draw(t, "steps")
		for i := range steps {
			w := writers.Draw(t, "writer")
			now := hlc.Time{Wall: int64(i + 2)}
			switch rapid.IntRange(0, 2).Draw(t, "op") {
			case 0:
				prev := e
				// The caller's envelope may carry anything; update rebuilds from prev.
				e.OriginInstanceID = "garbage"
				e.SyncVersion = -99
				StampUpdate(&e, prev, w, now)
				deleted = e.Deleted // update leaves Deleted as the caller set it
			case 1:
				StampBump(&e, w, now)
			case 2:
				StampTombstone(&e, w, now)
				deleted = true
			}
			writes++
			lastWriter = w
		}

		if e.SyncVersion != writes {
			t.Fatalf("SyncVersion = %d after %d writes", e.SyncVersion, writes)
		}
		if e.OriginInstanceID != origin {
			t.Fatalf("origin changed: %q -> %q", origin, e.OriginInstanceID)
		}
		if e.LastWriterID != lastWriter {
			t.Fatalf("LastWriterID = %q, want %q", e.LastWriterID, lastWriter)
		}
		if e.Deleted != deleted {
			t.Fatalf("Deleted = %v, want %v", e.Deleted, deleted)
		}
	})
}

// TestPropCAS pins the compare-and-set truth table over arbitrary versions: an
// expectation is honored iff it is zero (unconditional) or equals the stored
// version, and any non-zero expectation against nothing stored is a conflict.
func TestPropCAS(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		expected := rapid.Int64Range(0, 10).Draw(t, "expected")
		if hasStored := rapid.Bool().Draw(t, "hasStored"); hasStored {
			stored := Envelope{SyncVersion: rapid.Int64Range(1, 10).Draw(t, "stored")}
			want := expected == 0 || expected == stored.SyncVersion
			if got := CAS(expected, &stored); got != want {
				t.Fatalf("CAS(%d, stored %d) = %v, want %v", expected, stored.SyncVersion, got, want)
			}
		} else if got := CAS(expected, nil); got != (expected == 0) {
			t.Fatalf("CAS(%d, nil) = %v, want %v", expected, got, expected == 0)
		}
	})
}

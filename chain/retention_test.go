package chain_test

import (
	"testing"

	"github.com/ionalpha/flynn/chain"
	"github.com/ionalpha/flynn/spine"
)

// TestRetentionManifestCommitsToSet proves the manifest digest is a function of the set of
// content ids, independent of the order they are presented: the same ids in a different
// order yield the same digest, so a producer cannot change what it claims to have moved by
// reordering, and a verifier that rebuilds the set from storage need not match the mover's
// visitation order.
func TestRetentionManifestCommitsToSet(t *testing.T) {
	a := chain.RetentionManifest([]string{"c1", "c2", "c3"})
	reordered := chain.RetentionManifest([]string{"c3", "c1", "c2"})
	if a != reordered {
		t.Fatalf("manifest depends on order: %s != %s", a, reordered)
	}
}

// TestRetentionManifestSensitiveToMembership proves the digest changes when the set
// changes: dropping or adding a body yields a different manifest, so a record cannot claim
// to have archived one set while having moved another.
func TestRetentionManifestSensitiveToMembership(t *testing.T) {
	base := chain.RetentionManifest([]string{"c1", "c2", "c3"})
	dropped := chain.RetentionManifest([]string{"c1", "c2"})
	added := chain.RetentionManifest([]string{"c1", "c2", "c3", "c4"})
	if base == dropped {
		t.Fatal("manifest unchanged after dropping a content id")
	}
	if base == added {
		t.Fatal("manifest unchanged after adding a content id")
	}
}

// TestDecodeRetention round-trips a RetentionArchived event into a RetentionRecord and
// rejects an unrelated event. The integer fields are supplied as float64, the shape a JSON
// round trip through the store produces, so the decode must read them tolerantly.
func TestDecodeRetention(t *testing.T) {
	manifest := chain.RetentionManifest([]string{"a", "b"})
	e := spine.Event{
		Stream: chain.RetentionStream,
		Type:   chain.RetentionArchived,
		Actor:  spine.ActorSystem,
		Payload: map[string]any{
			chain.RetentionKeyAction:    chain.RetentionActionArchive,
			chain.RetentionKeyTier:      chain.RetentionTierWarm,
			chain.RetentionKeyMoved:     float64(2),
			chain.RetentionKeyHotBytes:  float64(4096),
			chain.RetentionKeyWarmBytes: float64(700),
			chain.RetentionKeyManifest:  manifest,
		},
	}
	rec, ok := chain.DecodeRetention(e)
	if !ok {
		t.Fatal("DecodeRetention rejected a RetentionArchived event")
	}
	if rec.Action != chain.RetentionActionArchive || rec.Tier != chain.RetentionTierWarm {
		t.Fatalf("decoded action/tier = %q/%q", rec.Action, rec.Tier)
	}
	if rec.Moved != 2 || rec.HotBytes != 4096 || rec.WarmBytes != 700 {
		t.Fatalf("decoded counts = moved %d hot %d warm %d", rec.Moved, rec.HotBytes, rec.WarmBytes)
	}
	if rec.Manifest != manifest {
		t.Fatalf("decoded manifest = %q, want %q", rec.Manifest, manifest)
	}

	if _, ok := chain.DecodeRetention(spine.Event{Type: "dispatch.start"}); ok {
		t.Fatal("DecodeRetention accepted a non-retention event")
	}
}

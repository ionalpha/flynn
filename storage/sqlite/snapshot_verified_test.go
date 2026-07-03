package sqlite_test

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/ionalpha/flynn/chain"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/resource/resourcetest"
	"github.com/ionalpha/flynn/storage/sqlite"
)

func testSealer(t testing.TB) *chain.SnapshotSealer {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = 0x6b
	}
	priv := ed25519.NewKeyFromSeed(seed)
	signer, err := chain.NewEd25519RootSigner("inst-1", priv)
	if err != nil {
		t.Fatal(err)
	}
	ring := chain.NewRootKeyring()
	if err := ring.Add("inst-1", priv.Public().(ed25519.PublicKey)); err != nil {
		t.Fatal(err)
	}
	sealer, err := chain.NewSnapshotSealer(signer, ring, nil)
	if err != nil {
		t.Fatal(err)
	}
	return sealer
}

// TestVerifiedSnapshotRebuild proves the durable backend's full snapshot loop:
// the automatic cadence writes a sealed snapshot, the snapshot opens (verifies)
// cleanly, a rebuild through it leaves the projection unchanged, and a tampered
// snapshot falls back to folding the whole stream with the identical result.
func TestVerifiedSnapshotRebuild(t *testing.T) {
	ctx := context.Background()
	reg := resourcetest.NewRegistry(t)
	sealer := testSealer(t)
	p, err := sqlite.Open(
		ctx, ":memory:",
		sqlite.WithSnapshotCodec(sealer),
		sqlite.WithSnapshotEvery(4),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	rs := p.Resources(reg)

	sizes := []string{"s", "m", "l"}
	for i := range 11 {
		if _, err := rs.Put(ctx, resource.Resource{
			APIVersion: "test.ionagent.io/v1", Kind: "Widget",
			Name: fmt.Sprintf("w-%d", i%5),
			Spec: json.RawMessage(fmt.Sprintf(`{"size":"%s","count":%d}`, sizes[i%3], i)),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := rs.Delete(ctx, "Widget", resource.Scope{}, "w-1"); err != nil {
		t.Fatal(err)
	}

	// The cadence (every 4 mutations, 12 total) must have checkpointed by itself,
	// and the stored snapshot must be a sealed envelope that verifies.
	snap, found, err := p.Log().LatestSnapshot(ctx, resource.ResourceStream, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("automatic cadence wrote no snapshot")
	}
	if _, err := sealer.Open(ctx, snap); err != nil {
		t.Fatalf("stored snapshot does not verify: %v", err)
	}

	// dump serializes the live projection normalized through a decode/encode
	// round trip, so raw JSON fields (Spec, Status) compare by structure. The
	// event fold re-encodes them with sorted keys while the live and snapshot
	// paths keep the caller's bytes; the content hash proves they are the same
	// value either way.
	dump := func() string {
		all, err := rs.ListAll(ctx, "Widget", nil)
		if err != nil {
			t.Fatal(err)
		}
		b, err := json.Marshal(all)
		if err != nil {
			t.Fatal(err)
		}
		var norm []map[string]any
		if err := json.Unmarshal(b, &norm); err != nil {
			t.Fatal(err)
		}
		nb, err := json.Marshal(norm)
		if err != nil {
			t.Fatal(err)
		}
		return string(nb)
	}
	before := dump()

	rebuilder, ok := rs.(interface{ Rebuild(context.Context) error })
	if !ok {
		t.Fatal("sqlite resource store should expose Rebuild")
	}
	if err := rebuilder.Rebuild(ctx); err != nil {
		t.Fatalf("rebuild from snapshot: %v", err)
	}
	if got := dump(); got != before {
		t.Fatalf("rebuild from snapshot changed the projection:\n got:  %s\n want: %s", got, before)
	}

	// Tamper gate: a bit-flipped snapshot must be rejected and the rebuild must
	// fall back to the full fold, ending in the same state.
	tampered := snap
	tampered.Payload = append([]byte(nil), snap.Payload...)
	tampered.Payload[len(tampered.Payload)/2] ^= 0x01
	if err := p.Log().SaveSnapshot(ctx, tampered); err != nil {
		t.Fatal(err)
	}
	if err := rebuilder.Rebuild(ctx); err != nil {
		t.Fatalf("rebuild with tampered snapshot: %v", err)
	}
	if got := dump(); got != before {
		t.Fatalf("tampered snapshot changed the rebuilt projection:\n got:  %s\n want: %s", got, before)
	}
}

// BenchmarkResourceRebuild is the two-point flatness gate for verified
// snapshots: with a snapshot at the head, rebuild cost must stay flat as the
// stream doubles, while the full fold grows linearly. The live set is fixed (8
// records, updated repeatedly), so only history length varies.
func BenchmarkResourceRebuild(b *testing.B) {
	for _, n := range []int{1000, 2000} {
		for _, mode := range []string{"fullfold", "snapshot"} {
			b.Run(fmt.Sprintf("%s-%d", mode, n), func(b *testing.B) {
				ctx := context.Background()
				reg := benchRegistry()
				sealer := testSealer(b)
				var opts []sqlite.Option
				if mode == "snapshot" {
					opts = append(opts, sqlite.WithSnapshotCodec(sealer))
				}
				p, err := sqlite.Open(ctx, ":memory:", opts...)
				if err != nil {
					b.Fatal(err)
				}
				defer func() { _ = p.Close() }()
				rs := p.Resources(reg)
				for i := range n {
					if _, err := rs.Put(ctx, resource.Resource{
						APIVersion: benchAPIVersion, Kind: "Gadget",
						Name: fmt.Sprintf("g-%d", i%8),
						Spec: json.RawMessage(fmt.Sprintf(`{"size":"s%d"}`, i)),
					}); err != nil {
						b.Fatal(err)
					}
				}
				if mode == "snapshot" {
					if err := rs.Snapshot(ctx); err != nil {
						b.Fatal(err)
					}
				}
				rebuilder := rs.(interface{ Rebuild(context.Context) error })
				b.ResetTimer()
				for range b.N {
					if err := rebuilder.Rebuild(ctx); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

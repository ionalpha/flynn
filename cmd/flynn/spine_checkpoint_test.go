package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/chain"
	"github.com/ionalpha/flynn/controlplane"
	"github.com/ionalpha/flynn/spine"
)

// TestSpineVerifyCheckpointedStream confirms `flynn spine verify` verifies a durably
// checkpointed stream (the served path) from its latest signed checkpoint, and reports a
// stream with no checkpoint rather than falsely passing it.
func TestSpineVerifyCheckpointedStream(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := openDataStore(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	// A self-certifying signer, so verify recovers the key from the checkpoint alone.
	seed := make([]byte, ed25519.SeedSize)
	seed[0] = 7
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)
	signer, err := chain.NewEd25519RootSigner(controlplane.PrincipalID(pub), priv)
	if err != nil {
		t.Fatal(err)
	}

	rec := chain.NewDurableRecorder(store.Log(),
		func(s string) chain.FlushNodeStore { return store.MerkleNodes(s) },
		store, signer, nil, 20)
	const stream = "server/spine"
	for range 65 {
		if _, err := rec.Append(ctx, spine.AppendInput{Stream: stream, Type: "ev", Actor: spine.ActorSystem}); err != nil {
			t.Fatal(err)
		}
	}
	if err := rec.Checkpoint(ctx, stream); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := verifyCheckpointedStream(&buf, store, stream); err != nil {
		t.Fatalf("verifying a checkpointed stream failed: %v\n%s", err, buf.String())
	}
	if !strings.Contains(buf.String(), "integrity:    VERIFIED") {
		t.Fatalf("expected integrity VERIFIED, got:\n%s", buf.String())
	}

	if err := verifyCheckpointedStream(&buf, store, "server/absent"); err == nil {
		t.Fatal("verified a stream that has no checkpoint")
	}
}

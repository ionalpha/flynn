package main

import (
	"context"
	"crypto/ed25519"
	"io"
	"testing"

	"github.com/ionalpha/flynn/chain"
	"github.com/ionalpha/flynn/controlplane"
	"github.com/ionalpha/flynn/harness"
	"github.com/ionalpha/flynn/llm/llmtest"
	"github.com/ionalpha/flynn/spine"
)

// TestSpineVerifyRoundTrip drives a real run, seals it, persists the signed record
// to the durable store, then reopens the store and verifies the run from it. It is
// the proof that persistence preserves the signed bytes: the record survives a
// SQLite round trip and still verifies, and the public key is recovered from the
// record's self-certifying key id with no shared state.
func TestSpineVerifyRoundTrip(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := openDataStore(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := missionRegistry()
	if err != nil {
		t.Fatal(err)
	}

	// A signer whose key id is a self-certifying principal id, so verifyRun recovers
	// the public key from the record alone. Built from a fixed seed so the test needs
	// no vault.
	seed := make([]byte, ed25519.SeedSize)
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)
	signer, err := chain.NewEd25519RootSigner(controlplane.PrincipalID(pub), priv)
	if err != nil {
		t.Fatal(err)
	}

	rec := chain.NewRecordingLog(store.Log(), nil)
	model := llmtest.NewScripted(llmtest.SayText("done"))
	_, runID, _, err := drive(ctx, io.Discard, model, harness.Plan{}, t.TempDir(),
		"reply done and stop", defaultSystemPrompt, store.Resources(reg), store.Jobs(), rec, false, "")
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	if err := sealRun(ctx, store, rec, runID, signer); err != nil {
		t.Fatalf("seal: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// Verify from the reopened durable store.
	if err := verifyRun(dataDir, runID); err != nil {
		t.Fatalf("verifying a persisted run failed: %v", err)
	}
}

// TestSpineVerifyRejectsUnsealed confirms verifyRun reports a run that was never
// sealed, and a run that does not exist, rather than reporting a false pass.
func TestSpineVerifyRejectsUnsealed(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := openDataStore(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Log().Append(ctx, spine.AppendInput{
		Stream: "run/x", Type: "noise", Actor: spine.ActorAgent,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	if err := verifyRun(dataDir, "run/x"); err == nil {
		t.Fatal("verified a run that was never sealed")
	}
	if err := verifyRun(dataDir, "does-not-exist"); err == nil {
		t.Fatal("verified a nonexistent run")
	}
}

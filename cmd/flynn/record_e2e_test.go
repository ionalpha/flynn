package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"io"
	"testing"

	"github.com/ionalpha/flynn/chain"
	"github.com/ionalpha/flynn/harness"
	"github.com/ionalpha/flynn/llm/llmtest"
)

// TestRunEmitsVerifiableRecord drives a real run to convergence through a recording
// log, then seals the run and verifies it. It is the end-to-end proof that the
// runtime, unchanged, produces events that a third party can verify: the same
// drive path the binary uses, with a scripted model standing in for a provider, and
// a verifier that trusts only the signing key. A single-byte tamper of the record
// must then fail.
func TestRunEmitsVerifiableRecord(t *testing.T) {
	ctx := context.Background()
	store, err := openStore(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	reg, err := missionRegistry()
	if err != nil {
		t.Fatal(err)
	}

	// Record every event the run appends to the spine, without changing how the run
	// produces them.
	rec := chain.NewRecordingLog(store.Log(), nil)

	// A scripted model that answers and stops, so the run converges deterministically
	// with no provider or network.
	model := llmtest.NewScripted(llmtest.SayText("done"))

	result, runID, _, err := drive(ctx, io.Discard, model, harness.Plan{}, t.TempDir(),
		"reply done and stop", defaultSystemPrompt,
		store.Resources(reg), store.Jobs(), rec, false, "", nil)
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	if result == "" {
		t.Fatal("run produced no result")
	}

	// Sign the run's sealed checkpoint with a fixed test key (no randomness needed).
	seed := make([]byte, ed25519.SeedSize)
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)
	signer, err := chain.NewEd25519RootSigner("inst", priv)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := rec.Seal(runID, signer)
	if err != nil {
		t.Fatalf("seal run %q: %v", runID, err)
	}
	record, err := sealed.Marshal()
	if err != nil {
		t.Fatal(err)
	}

	ring := chain.NewRootKeyring()
	if err := ring.Add("inst", pub); err != nil {
		t.Fatal(err)
	}
	events, err := chain.VerifyRun(record, ring)
	if err != nil {
		t.Fatalf("verifying a real run's record failed: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("verified record carried no events")
	}

	bad := append([]byte{}, record...)
	bad[len(bad)/2] ^= 0xff
	if !bytes.Equal(bad, record) {
		if _, err := chain.VerifyRun(bad, ring); err == nil {
			t.Fatal("a tampered run record still verified")
		}
	}
}

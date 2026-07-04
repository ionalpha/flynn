package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"io"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/chain"
	"github.com/ionalpha/flynn/controlplane"
	"github.com/ionalpha/flynn/harness"
	"github.com/ionalpha/flynn/llm/llmtest"
)

// selfCertifyingSigner returns a signer whose key id is the self-certifying principal id
// of its public key, so a record it signs verifies from the durable store alone (the
// path the interactive /seal and /verify take). The seed is fixed, so the test needs no
// randomness.
func selfCertifyingSigner(t *testing.T) chain.RootSigner {
	t.Helper()
	priv := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	pub := priv.Public().(ed25519.PublicKey)
	signer, err := chain.NewEd25519RootSigner(controlplane.PrincipalID(pub), priv)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

// TestSealFromStoreProducesVerifiableRecord drives a real run to convergence, seals it
// from the durable store (the in-process path /seal takes, with no live recorder), and
// verifies the sealed record from the store alone. It proves the in-session seal and
// verify produce and check a record with the same integrity and governance guarantees
// as the one-shot runner's.
func TestSealFromStoreProducesVerifiableRecord(t *testing.T) {
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

	model := llmtest.NewScripted(llmtest.SayText("done"))
	_, runID, _, err := drive(ctx, io.Discard, model, harness.Plan{}, t.TempDir(),
		"reply done and stop", defaultSystemPrompt,
		store.Resources(reg), store.Jobs(), store.Log(), false, "", nil)
	if err != nil {
		t.Fatalf("drive: %v", err)
	}

	signer := selfCertifyingSigner(t)

	// Verifying before sealing reports the run carries no record, not a false pass.
	if err := verifyStoredRun(ctx, io.Discard, store, runID); err == nil {
		t.Fatal("verify of an unsealed run should error")
	}

	if err := sealRunFromStore(ctx, store, runID, signer); err != nil {
		t.Fatalf("seal: %v", err)
	}

	var buf bytes.Buffer
	if err := verifyStoredRun(ctx, &buf, store, runID); err != nil {
		t.Fatalf("verify after seal: %v\n%s", err, buf.String())
	}
	report := buf.String()
	for _, want := range []string{"integrity:", "VERIFIED", "governance:", "OK"} {
		if !strings.Contains(report, want) {
			t.Errorf("report missing %q:\n%s", want, report)
		}
	}

	// Re-sealing re-folds the same events (skipping the earlier record event) and still
	// verifies, so a second /seal is harmless.
	if err := sealRunFromStore(ctx, store, runID, signer); err != nil {
		t.Fatalf("re-seal: %v", err)
	}
	if err := verifyStoredRun(ctx, io.Discard, store, runID); err != nil {
		t.Fatalf("verify after re-seal: %v", err)
	}
}

// TestSealEmptyRunRefused proves sealing a stream with no events is refused rather than
// producing a record that attests nothing.
func TestSealEmptyRunRefused(t *testing.T) {
	ctx := context.Background()
	store, err := openStore(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	if err := sealRunFromStore(ctx, store, "nonesuch", selfCertifyingSigner(t)); err == nil {
		t.Fatal("sealing a run with no events should error")
	}
}

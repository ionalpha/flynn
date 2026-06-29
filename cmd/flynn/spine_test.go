package main

import (
	"context"
	"crypto/ed25519"
	"io"
	"testing"

	"github.com/ionalpha/flynn/chain"
	"github.com/ionalpha/flynn/controlplane"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/harness"
	"github.com/ionalpha/flynn/llm/llmtest"
	"github.com/ionalpha/flynn/spine"
)

// TestRunRecordsGovernanceOnSpine drives a real run and asserts the governed
// actions' admission events land on the run's own stream, so the admission record is
// part of the run's recorded history (and therefore its sealed record) rather than
// only the live trace. Each model call is admitted through the dispatch waist, which
// emits a start event carrying the action's trust level.
func TestRunRecordsGovernanceOnSpine(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := openDataStore(ctx, dataDir)
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
		"reply done and stop", defaultSystemPrompt, store.Resources(reg), store.Jobs(), store.Log(), false, "", nil)
	if err != nil {
		t.Fatalf("drive: %v", err)
	}

	events, err := store.Log().Read(ctx, spine.Query{Stream: runID})
	if err != nil {
		t.Fatal(err)
	}
	var starts, ends int
	for _, e := range events {
		switch e.Type {
		case dispatch.EventStart:
			starts++
			if _, ok := e.Payload["trust"].(string); !ok {
				t.Errorf("%s event missing a trust level", e.Type)
			}
		case dispatch.EventEnd:
			ends++
		}
	}
	if starts == 0 {
		t.Fatal("no admission (dispatch.start) events recorded on the run stream")
	}
	if ends == 0 {
		t.Fatal("no completion (dispatch.end) events recorded on the run stream")
	}

	// The governance the run actually emitted satisfies the admission invariants: no
	// action completed without admission, none denied yet executed. This is the
	// end-to-end check that what the runtime records is what the verifier accepts.
	if err := chain.VerifyGovernance(events); err != nil {
		t.Fatalf("the run's own governance did not verify: %v", err)
	}
}

// TestGovernanceVocabularyMatchesDispatch guards against the chain verifier's
// governance event-type vocabulary drifting from what the dispatch waist emits. They
// are defined in separate packages on purpose (the verifier does not import the
// waist), so this is the integration point that keeps them in agreement.
func TestGovernanceVocabularyMatchesDispatch(t *testing.T) {
	if chain.GovStart != dispatch.EventStart ||
		chain.GovEnd != dispatch.EventEnd ||
		chain.GovRejected != dispatch.EventRejected {
		t.Fatal("chain governance vocabulary has drifted from the dispatch event types")
	}
}

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
		"reply done and stop", defaultSystemPrompt, store.Resources(reg), store.Jobs(), rec, false, "", nil)
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

// TestRunVerificationIndependent confirms the verification command's verdict is the
// command's real exit code, run in a sandbox and not derived from anything the model
// said.
func TestRunVerificationIndependent(t *testing.T) {
	ctx := context.Background()
	if !runVerification(ctx, t.TempDir(), "exit 0") {
		t.Fatal("a passing command was reported as failed")
	}
	if runVerification(ctx, t.TempDir(), "exit 1") {
		t.Fatal("a failing command was reported as passed")
	}
}

// TestRunGroundTruthEndToEnd drives a real run, grounds its success in an independent
// check, seals it, and verifies the sealed record. A run whose check passed is
// grounded; a run whose check failed claims success the record can show is not backed,
// so the run's own sealed record fails the ground-truth check rather than overstating
// the outcome.
func TestRunGroundTruthEndToEnd(t *testing.T) {
	groundTruthOf := func(t *testing.T, verify string) error {
		t.Helper()
		ctx := context.Background()
		dataDir := t.TempDir()
		store, err := openDataStore(ctx, dataDir)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = store.Close() }()
		reg, err := missionRegistry()
		if err != nil {
			t.Fatal(err)
		}

		seed := make([]byte, ed25519.SeedSize)
		priv := ed25519.NewKeyFromSeed(seed)
		pub := priv.Public().(ed25519.PublicKey)
		keyID := controlplane.PrincipalID(pub)
		signer, err := chain.NewEd25519RootSigner(keyID, priv)
		if err != nil {
			t.Fatal(err)
		}

		rec := chain.NewRecordingLog(store.Log(), nil)
		model := llmtest.NewScripted(llmtest.SayText("done"))
		_, runID, _, err := drive(ctx, io.Discard, model, harness.Plan{}, t.TempDir(),
			"reply done and stop", defaultSystemPrompt, store.Resources(reg), store.Jobs(), rec, false, "", nil)
		if err != nil {
			t.Fatalf("drive: %v", err)
		}
		recordGroundTruth(ctx, io.Discard, rec, runID, t.TempDir(), verify)
		if err := sealRun(ctx, store, rec, runID, signer); err != nil {
			t.Fatalf("seal: %v", err)
		}

		events, err := store.Log().Read(ctx, spine.Query{Stream: runID})
		if err != nil {
			t.Fatal(err)
		}
		record, err := recordFromEvents(events)
		if err != nil {
			t.Fatalf("record: %v", err)
		}
		ring := chain.NewRootKeyring()
		if err := ring.Add(keyID, pub); err != nil {
			t.Fatal(err)
		}
		runEvents, err := chain.VerifyRun(record, ring)
		if err != nil {
			t.Fatalf("verify run: %v", err)
		}
		return chain.VerifyGroundTruth(runEvents)
	}

	t.Run("passing check grounds the run", func(t *testing.T) {
		if err := groundTruthOf(t, "exit 0"); err != nil {
			t.Fatalf("a run with a passing independent check was not grounded: %v", err)
		}
	})
	t.Run("failing check is not grounded", func(t *testing.T) {
		if err := groundTruthOf(t, "exit 1"); err == nil {
			t.Fatal("a run whose independent check failed was accepted as grounded")
		}
	})
}

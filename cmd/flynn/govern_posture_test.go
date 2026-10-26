package main

import (
	"context"
	"testing"
	"time"

	"github.com/ionalpha/flynn/llm/llmtest"
)

// TestAssemblyPlanningAndProofReachThePosture: the binary's planning and ledger-proof
// switches are carried into the governed assembly, which reports them on the run's posture.
func TestAssemblyPlanningAndProofReachThePosture(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store := memStore(t)
	run, err := assembly{
		model: llmtest.NewScripted(llmtest.SayText("done")), workdir: t.TempDir(), system: defaultSystemPrompt,
		rstore: store.Resources(mustRegistry(t)), jq: store.Jobs(), log: store.Log(),
		planning: true, proof: true,
	}.assemble(ctx)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	defer func() { _ = run.Close() }()
	if p := run.Posture(); !p.Verifier || !p.LedgerProof {
		t.Fatalf("posture = %+v, want a verifier and ledger proof", p)
	}
}

// TestUnrecordedAttestedWithoutAnExternalAgent: a native run has no external harness, so
// it has nothing unrecorded and no error.
func TestUnrecordedAttestedWithoutAnExternalAgent(t *testing.T) {
	if n, err := unrecordedAttested(nil); n != 0 || err != nil {
		t.Fatalf("unrecordedAttested(nil) = (%d, %v), want (0, nil)", n, err)
	}
}

package session

import (
	"context"
	"crypto/ed25519"
	"testing"

	"github.com/ionalpha/flynn/approval"
	"github.com/ionalpha/flynn/bus"
	"github.com/ionalpha/flynn/chain"
	"github.com/ionalpha/flynn/internal/spinesink"
	"github.com/ionalpha/flynn/spine"
)

// TestSealedRunWithHeadlessKindsVerifies seals a run that holds every kind the headless
// vocabulary adds and checks that the record verifies and that the governance check, which
// an older verifier runs, passes over it. An approval request now sits in the sealed record
// beside the dispatch events, so this is the test that fails if a new type breaks a reader
// that has never heard of it.
func TestSealedRunWithHeadlessKindsVerifies(t *testing.T) {
	ctx := context.Background()
	rl := chain.NewRecordingLog(spine.NewMemoryLog(), nil)
	s := New(rl, bus.NewMemory(), WithID("run-1"))
	sink := spinesink.NewApproval(rl, "run-1")

	s.AttachClient(ctx, ClientAttachment{ID: "c1", Major: 1, Transport: "stdio"})
	if _, err := rl.Append(ctx, spine.AppendInput{
		Stream: "run-1", Type: chain.GovStart, Actor: spine.ActorAgent,
		Payload: map[string]any{chain.GovCallKey: int64(1), chain.GovActionKey: "shell"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := sink.Request(ctx, spinesink.Request{ID: "r1", Action: "shell", Host: "box"}); err != nil {
		t.Fatal(err)
	}
	if err := sink.Answer(ctx, spinesink.Answer{RequestID: "r1", Action: "shell", Host: "box", Reason: approval.ReasonAllowed}); err != nil {
		t.Fatal(err)
	}
	if _, err := rl.Append(ctx, spine.AppendInput{
		Stream: "run-1", Type: chain.GovEnd, Actor: spine.ActorAgent,
		Payload: map[string]any{chain.GovCallKey: int64(1)},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.ExtendBudget(ctx, Ceiling{Tokens: 10}, Ceiling{Tokens: 20}); err != nil {
		t.Fatal(err)
	}
	s.EndPrompt(ctx, "p1", PromptEnd{Stop: StopConverged})

	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := chain.NewEd25519RootSigner("inst", priv)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := rl.Seal("run-1", signer)
	if err != nil {
		t.Fatalf("seal: %v", err)
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
		t.Fatalf("a sealed run holding the new kinds does not verify: %v", err)
	}

	seen := map[string]bool{}
	for _, e := range events {
		seen[e.Type] = true
	}
	for _, typ := range []string{
		"client.attached", "approval.requested", "approval.decision", "budget.extended", "prompt.ended",
	} {
		if !seen[typ] {
			t.Errorf("the sealed record has no %s event", typ)
		}
	}
	if err := chain.VerifyGovernance(events); err != nil {
		t.Fatalf("the governance check does not tolerate the new kinds: %v", err)
	}
}

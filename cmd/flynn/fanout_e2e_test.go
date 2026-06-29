package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/chain"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/harness"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/mission"
)

// fanoutChildMarker is the token the parent puts in a delegated sub-goal's objective
// so the test model recognises a child run and answers it directly.
const fanoutChildMarker = "CHILD-TASK"

// fanoutTestModel is a deterministic model for the fan-out e2e: it answers from the
// request content rather than a fixed sequence, so it behaves correctly whether the
// parent run or a delegated child calls it, in any interleaving (the two run
// concurrently and share this one model). The parent delegates once through the spawn
// tool, then folds the child's answer and finishes; a child, recognised by the marker
// its parent put in the objective, answers directly.
type fanoutTestModel struct{}

var _ llm.Model = fanoutTestModel{}

func (fanoutTestModel) Generate(_ context.Context, req llm.Request) (llm.Response, error) {
	// Once a tool result is in the transcript the parent has folded its child: finish.
	for _, m := range req.Messages {
		for _, b := range m.Blocks {
			if b.Kind == llm.KindToolResult {
				return llm.Response{Message: llm.Text(llm.RoleAssistant, "folded the child and finished"), StopReason: llm.StopEndTurn}, nil
			}
		}
	}
	// A child recognises itself by the marker in its objective and answers.
	if strings.Contains(messageText(req.Messages), fanoutChildMarker) {
		return llm.Response{Message: llm.Text(llm.RoleAssistant, "child finished "+fanoutChildMarker), StopReason: llm.StopEndTurn}, nil
	}
	// The parent's first turn: delegate one self-contained sub-goal, requesting only
	// the write tool. The child still gets the model call (the spawner always grants
	// it), so this exercises that path too.
	return llm.Response{
		Message: llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{
			Kind: llm.KindToolUse,
			ToolUse: &llm.ToolUse{
				ID:    "spawn-1",
				Name:  mission.ActionSpawn,
				Input: json.RawMessage(`{"objective":"` + fanoutChildMarker + `: produce the answer","actions":["write"]}`),
			},
		}}},
		StopReason: llm.StopToolUse,
	}, nil
}

// messageText concatenates the text of a request's messages, the signal the test
// model branches on.
func messageText(msgs []llm.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		b.WriteString(m.TextContent())
		b.WriteByte(' ')
	}
	return b.String()
}

// TestFanoutRunSealsOneVerifiableRecord is the end-to-end proof of the fan-out path:
// a run with delegation enabled drives the goals engine over the durable store, the
// model delegates a sub-goal to a concurrent child agent, and the parent and child
// fold into one event stream that seals into a single signed record a third party can
// verify. It asserts the delegation actually happened (a child goal owned by the
// parent exists) and that a single-byte tamper of the sealed record fails to verify.
func TestFanoutRunSealsOneVerifiableRecord(t *testing.T) {
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

	// Record every event the run appends, without changing how it produces them.
	rec := chain.NewRecordingLog(store.Log(), nil)

	// Fan-out enabled. Children name no model, so the resolver is never consulted; a
	// nil resolver is the n=0 case of per-goal model routing.
	fc := &fanoutConfig{}

	result, runID, _, err := drive(ctx, io.Discard, fanoutTestModel{}, harness.Plan{}, t.TempDir(),
		"delegate the work to a child agent and then finish", defaultSystemPrompt,
		store.Resources(reg), store.Jobs(), rec, false, "", fc)
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	if result == "" {
		t.Fatal("run produced no result")
	}

	// The delegation really happened: a child goal owned by the parent run exists.
	goals, err := store.Resources(reg).ListAll(ctx, goal.Kind, nil)
	if err != nil {
		t.Fatal(err)
	}
	children := 0
	for _, g := range goals {
		for _, o := range g.OwnerReferences {
			if o.Kind == goal.Kind && o.Controller {
				children++
			}
		}
	}
	if children == 0 {
		t.Fatalf("fan-out enabled but no child goal was spawned (have %d goals)", len(goals))
	}

	// Seal the run's checkpoint with a fixed test key (no randomness needed) and verify
	// it as a third party would, trusting only the signing key.
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
		t.Fatalf("verifying the fan-out run's record failed: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("verified record carried no events")
	}

	bad := append([]byte{}, record...)
	bad[len(bad)/2] ^= 0xff
	if !bytes.Equal(bad, record) {
		if _, err := chain.VerifyRun(bad, ring); err == nil {
			t.Fatal("a tampered fan-out record still verified")
		}
	}
}

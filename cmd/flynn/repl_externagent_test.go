package main

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/externagent"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/resource"
)

// extSession builds an interactive session driven by the named external backend, over a
// real store, with a run already opened. The driver is built from the real adapter (its
// Command and Parse are what a turn would use); no episode is run here, so no CLI is
// needed: what these tests exercise is the session's own half of a turn, which is what
// the session owns. The episode half (continuing the CLI's conversation, putting only the
// new turn to it) is proven against a real bridge in the externagent driver tests.
func extSession(t *testing.T, backend, cliModel string) *replSession {
	t.Helper()
	ctx := context.Background()
	store, err := openDataStore(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	reg, err := missionRegistry()
	if err != nil {
		t.Fatal(err)
	}

	var adapter externagent.Adapter
	switch backend {
	case "codex":
		adapter = externagent.NewCodex("", nil)
	default:
		adapter = externagent.NewClaude("", nil)
	}
	return &replSession{
		out:       io.Discard,
		ext:       &externAgent{model: cliModel, driver: externagent.NewDriver(adapter, nil, t.TempDir())},
		cwd:       t.TempDir(),
		store:     store,
		reg:       reg,
		dataDir:   t.TempDir(),
		modelSpec: backend + ":" + cliModel,
	}
}

// openExternalGoal puts the run's goal in the store the way the session's first turn
// does: an objective, the CLI's model, and a settled episode that reports the
// conversation the CLI opened.
func openExternalGoal(t *testing.T, s *replSession, runID, session string) {
	t.Helper()
	ctx := context.Background()
	spec, err := json.Marshal(goal.Spec{Objective: "the opening line", StopCondition: "done", Model: s.ext.model})
	if err != nil {
		t.Fatal(err)
	}
	status, err := json.Marshal(goal.Status{
		Phase:      goal.PhaseConverged,
		Checkpoint: json.RawMessage(`{"done":true,"result":"answer one","session":"` + session + `"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.Resources(s.reg).Put(ctx, resource.Resource{
		APIVersion: goal.GroupVersion, Kind: goal.Kind, Name: runID, Spec: spec, Status: status,
	}); err != nil {
		t.Fatal(err)
	}
	s.started = true
	s.runID = runID
}

// externalGoal reads the run's goal back.
func externalGoal(t *testing.T, s *replSession) (goal.Spec, goal.Status) {
	t.Helper()
	r, err := s.store.Resources(s.reg).Get(context.Background(), goal.Kind, resource.Scope{}, s.runID)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := goal.DecodeSpec(r)
	if err != nil {
		t.Fatal(err)
	}
	status, err := goal.DecodeStatus(r)
	if err != nil {
		t.Fatal(err)
	}
	return spec, status
}

// TestExternalTurnContinuesTheHarnessConversation: a second line typed into a claude or
// codex session reopens the same durable run and hands the CLI back the conversation it
// opened, so the harness answers with the context of the earlier turns. The objective is
// left alone: it records what the run set out to do, and a turn is not a new objective.
func TestExternalTurnContinuesTheHarnessConversation(t *testing.T) {
	s := extSession(t, "claude", "claude-opus-4-8")
	openExternalGoal(t, s, "run-1", "sess-abc")

	if err := s.reopen(context.Background(), "and now the second line", nil); err != nil {
		t.Fatalf("reopen: %v", err)
	}

	spec, status := externalGoal(t, s)
	if status.Phase == goal.PhaseConverged {
		t.Error("a reopened turn is still converged, so nothing would drive it")
	}
	var cp struct {
		Done    bool   `json:"done"`
		Session string `json:"session"`
		Input   string `json:"input"`
	}
	if err := json.Unmarshal(status.Checkpoint, &cp); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if cp.Done {
		t.Error("the reopened episode is still marked done")
	}
	if cp.Session != "sess-abc" {
		t.Errorf("the turn dropped the CLI's conversation (%q): the harness would answer with no memory of the session", cp.Session)
	}
	if cp.Input != "and now the second line" {
		t.Errorf("the turn's input is %q, want the line the user just typed", cp.Input)
	}
	if spec.Objective != "the opening line" {
		t.Errorf("the turn overwrote the run's objective: %q", spec.Objective)
	}
}

// TestExternalModelSwitchRetargetsTheNextTurn: /model claude:<other> inside a claude
// session changes the model the CLI drives from the next turn on, without disturbing the
// conversation the CLI holds. Switching the model is not a reason to lose the context.
func TestExternalModelSwitchRetargetsTheNextTurn(t *testing.T) {
	s := extSession(t, "claude", "claude-opus-4-8")
	openExternalGoal(t, s, "run-1", "sess-abc")

	if err := s.switchModel(context.Background(), []string{"claude:claude-sonnet-4-5"}, io.Discard); err != nil {
		t.Fatalf("switchModel: %v", err)
	}
	if err := s.reopen(context.Background(), "next line", nil); err != nil {
		t.Fatalf("reopen: %v", err)
	}

	spec, status := externalGoal(t, s)
	if spec.Model != "claude-sonnet-4-5" {
		t.Errorf("the next turn drives %q, want the model just switched to", spec.Model)
	}
	if !strings.Contains(string(status.Checkpoint), "sess-abc") {
		t.Errorf("switching the model dropped the CLI's conversation: %s", status.Checkpoint)
	}
}

// TestExternalSessionRefusesAHarnessSwap: a record declares the one harness that drove
// the run. Swapping the harness mid-run (to the other backend, or to a native model)
// would seal a record whose declaration is true of only part of it, so it is refused with
// a way out rather than quietly producing that record.
func TestExternalSessionRefusesAHarnessSwap(t *testing.T) {
	for _, spec := range []string{"codex", "anthropic:claude-opus-4-8"} {
		s := extSession(t, "claude", "claude-opus-4-8")
		openExternalGoal(t, s, "run-1", "sess-abc")

		err := s.switchModel(context.Background(), []string{spec}, io.Discard)
		if err == nil {
			t.Fatalf("/model %s mid-run was allowed; the run's record would declare a harness that drove only part of it", spec)
		}
		if !strings.Contains(err.Error(), "new session") {
			t.Errorf("/model %s: the refusal does not name the way out: %v", spec, err)
		}
		// The session keeps driving what it was driving.
		if s.ext == nil || s.ext.driver.Name() != "claude" {
			t.Errorf("/model %s: the refused switch changed the session's harness anyway", spec)
		}
	}
}

// TestExternalSessionRefusesImages: a turn reaches the CLI as text on its stdin, so an
// attachment has nowhere to go. Refusing is the honest answer; dropping it silently would
// leave the user reasoning about an image the agent never saw.
func TestExternalSessionRefusesImages(t *testing.T) {
	s := extSession(t, "claude", "claude-opus-4-8")
	_, err := s.runTurn(context.Background(), "look at this", []llm.Image{{MediaType: "image/png", Data: []byte{0x89}}}, nil)
	if err == nil {
		t.Fatal("an image attachment was accepted by a session that cannot carry it")
	}
	if !strings.Contains(err.Error(), "text turns only") {
		t.Errorf("the refusal does not say why: %v", err)
	}
}

// TestExternalCompactIsRefused: the conversation being compacted is the CLI's, and it
// manages its own context. Compacting a transcript the harness will never read would be a
// silent no-op dressed up as a saving.
func TestExternalCompactIsRefused(t *testing.T) {
	s := extSession(t, "claude", "claude-opus-4-8")
	openExternalGoal(t, s, "run-1", "sess-abc")
	s.transcript = append(s.transcript, llm.Message{Role: llm.RoleUser})

	if _, err := s.compact(context.Background()); err == nil {
		t.Fatal("/compact was accepted in an external session")
	} else if !strings.Contains(err.Error(), "manages its own context") {
		t.Errorf("the refusal does not say why: %v", err)
	}
}

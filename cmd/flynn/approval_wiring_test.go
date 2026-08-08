package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/harness"
	"github.com/ionalpha/flynn/llm/llmtest"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/spine"
)

// stubPrompter stands in for the operator at the terminal: it answers every request the
// same way and records what it was asked, so a test can assert the run actually stopped
// to ask rather than inferring it from the outcome.
type stubPrompter struct {
	decision mission.ApprovalDecision
	asked    chan mission.ApprovalRequest
}

func newStubPrompter(dec mission.ApprovalDecision) *stubPrompter {
	return &stubPrompter{decision: dec, asked: make(chan mission.ApprovalRequest, 8)}
}

func (p *stubPrompter) Prompt(_ context.Context, req mission.ApprovalRequest) (mission.ApprovalDecision, error) {
	p.asked <- req
	return p.decision, nil
}

// TestApprovalAllowedActionRunsAndIsRecorded: a run whose policy lists the write tool
// stops and asks before writing, the operator's allow lets the action through, and the
// authorization lands on the run's own stream as a granted decision.
func TestApprovalAllowedActionRunsAndIsRecorded(t *testing.T) {
	prompter := newStubPrompter(mission.ApprovalDecision{Allow: true})
	dir, log, stream := driveGatedRun(t, prompter)

	select {
	case req := <-prompter.asked:
		if req.Action != "write" {
			t.Fatalf("the run paused on %q, want the listed action write", req.Action)
		}
	default:
		t.Fatal("the run never stopped to ask: a listed action was taken with no approval")
	}
	if _, err := os.Stat(filepath.Join(dir, "hello.txt")); err != nil {
		t.Fatalf("an approved action did not run: %v", err)
	}
	if d := lastApprovalDecision(t, log, stream); d["verdict"] != "granted" {
		t.Fatalf("the granted authorization was not recorded: %v", d)
	}
}

// TestApprovalDeclinedActionIsRefusedAndRecorded: declining stops the action. The file is
// not written, and the refusal is on the record rather than only in the model's context,
// so an audit afterwards can tell a refused attempt from one never made.
func TestApprovalDeclinedActionIsRefusedAndRecorded(t *testing.T) {
	prompter := newStubPrompter(mission.ApprovalDecision{Allow: false, Feedback: "not this file"})
	dir, log, stream := driveGatedRun(t, prompter)

	select {
	case <-prompter.asked:
	default:
		t.Fatal("the run never stopped to ask")
	}
	if _, err := os.Stat(filepath.Join(dir, "hello.txt")); err == nil {
		t.Fatal("a declined action ran anyway: the write went through after the operator refused it")
	}
	// The gate records the denial it reached before the prompter was ever consulted. That
	// is the row an auditor reads: the action was asked for and was not authorized.
	if d := lastApprovalDecision(t, log, stream); d["verdict"] != "denied" {
		t.Fatalf("the refusal was not recorded: %v", d)
	}
}

// TestApprovalWithNoPrompterRefusesTheAction is the non-interactive answer, and it is the
// one that has to be a decision rather than an accident: a run with a policy and nobody to
// ask refuses the listed action instead of taking it.
func TestApprovalWithNoPrompterRefusesTheAction(t *testing.T) {
	dir, log, stream := driveGatedRun(t, nil)

	if _, err := os.Stat(filepath.Join(dir, "hello.txt")); err == nil {
		t.Fatal("a run with no way to ask a person took the action anyway")
	}
	if d := lastApprovalDecision(t, log, stream); d["verdict"] != "denied" {
		t.Fatalf("the refusal was not recorded: %v", d)
	}
}

// TestUngatedRunIsUnchanged: with no policy the run is exactly what it was. Nothing pauses,
// nothing is asked, and no approval event is written, so the default install pays nothing
// for a mechanism it has not turned on.
func TestUngatedRunIsUnchanged(t *testing.T) {
	prompter := newStubPrompter(mission.ApprovalDecision{Allow: false})
	dir, log, stream := driveRun(t, approvalSetup{prompter: prompter})

	if _, err := os.Stat(filepath.Join(dir, "hello.txt")); err != nil {
		t.Fatalf("an ungated run did not perform its write: %v", err)
	}
	select {
	case req := <-prompter.asked:
		t.Fatalf("an ungated run stopped to ask about %q", req.Action)
	default:
	}
	if events := approvalEvents(t, log, stream); len(events) != 0 {
		t.Fatalf("an ungated run recorded %d approval decisions", len(events))
	}
}

// TestGatedRunAnnouncesItselfAndRefuses is the whole CLI path, the way `flynn goal
// --require-approval write` takes it: the option carries the policy through drive, the run
// says up front that it will stop and on what, and with nobody to ask it refuses the write
// rather than taking it. A run that halted with no warning would read as a hang, and one
// that refused an action because nobody could be asked should have said so before it began.
func TestGatedRunAnnouncesItselfAndRefuses(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dir := t.TempDir()
	model := llmtest.NewScripted(
		llmtest.CallTool("c1", "write", json.RawMessage(`{"path":"hello.txt","content":"hi"}`)),
		llmtest.SayText("could not write"),
	)
	var out bytes.Buffer
	if _, err := runLearningMission(ctx, &out, model, harness.Plan{}, nil, dir,
		"create hello.txt", "", memStore(t), nil, false, nil,
		withApproval([]string{"write"}, nil)); err != nil {
		t.Fatalf("run: %v", err)
	}

	printed := out.String()
	if !strings.Contains(printed, "approval required") || !strings.Contains(printed, "write") {
		t.Fatalf("the run did not say it was gated before it started:\n%s", printed)
	}
	// A one-shot run has no operator at a prompt, so the line has to say which of the two
	// shapes this is rather than implying somebody will be asked.
	if !strings.Contains(printed, "nothing here can prompt") {
		t.Fatalf("the run implied it would prompt when nothing could:\n%s", printed)
	}
	if _, err := os.Stat(filepath.Join(dir, "hello.txt")); err == nil {
		t.Fatal("a gated run with nobody to ask took the action anyway")
	}

	// The same run with someone to ask says the other thing, because the two are a
	// different experience: one is going to stop and wait, the other is going to refuse.
	dir2 := t.TempDir()
	var out2 bytes.Buffer
	if _, err := runLearningMission(ctx, &out2, llmtest.NewScripted(llmtest.SayText("nothing to do")),
		harness.Plan{}, nil, dir2, "do nothing", "", memStore(t), nil, false, nil,
		withApproval([]string{"write"}, newStubPrompter(mission.ApprovalDecision{Allow: true}))); err != nil {
		t.Fatalf("run with a prompter: %v", err)
	}
	if !strings.Contains(out2.String(), "paused for your decision") {
		t.Fatalf("a run that can prompt did not say so:\n%s", out2.String())
	}
}

// driveGatedRun drives one run whose policy requires approval for the write tool, with the
// given prompter (nil for a run with nobody to ask), and returns the working directory the
// write would have landed in and the log its decisions were recorded on.
func driveGatedRun(t *testing.T, prompter mission.ApprovalPrompter) (string, spine.Log, string) {
	t.Helper()
	return driveRun(t, approvalSetup{actions: []string{"write"}, prompter: prompter})
}

// driveRun assembles the shipped single-conversation runtime under appr and drives one goal
// through it with a scripted model that writes a file and then reports done. It returns the
// working directory and the run's log so the caller can assert on both what happened and
// what was recorded.
func driveRun(t *testing.T, appr approvalSetup) (string, spine.Log, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dir := t.TempDir()
	store := memStore(t)
	reg := mustRegistry(t)
	rstore := store.Resources(reg)
	log := store.Log()

	model := llmtest.NewScripted(
		llmtest.CallTool("c1", "write", json.RawMessage(`{"path":"hello.txt","content":"hi"}`)),
		llmtest.SayText("done"),
	)
	run, err := assembleMission(model, harness.Plan{}, dir, defaultSystemPrompt,
		rstore, store.Jobs(), log, nil, "", sandbox.ResourceLimits{}, false, false, appr)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	t.Cleanup(func() { _ = run.Close() })

	done := make(chan struct{})
	go func() { _ = run.rt.Start(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	if _, err := run.rt.SubmitGoal(ctx, run.sess.ID(), goal.Spec{
		Objective:     "write hello.txt",
		StopCondition: "the file is written",
	}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	waitForSettled(ctx, t, rstore, run.sess.ID())
	return dir, log, run.sess.ID()
}

// waitForSettled polls until the goal stops running, either way. A refused action is a
// legitimate ending here, so the helper does not insist on convergence: what the tests
// assert is what the run did and what it wrote down, not which phase it landed in.
func waitForSettled(ctx context.Context, t *testing.T, s resource.Store, name string) {
	t.Helper()
	for {
		r, err := s.Get(ctx, goal.Kind, resource.Scope{}, name)
		if err == nil {
			st, derr := goal.DecodeStatus(r)
			if derr != nil {
				t.Fatalf("decode status: %v", derr)
			}
			if st.Phase == goal.PhaseConverged || st.Phase == goal.PhaseStalled {
				return
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal("the run never settled")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// approvalEvents reads every authorization decision recorded across the log.
func approvalEvents(t *testing.T, log spine.Log, stream string) []spine.Event {
	t.Helper()
	events, err := log.Read(context.Background(), spine.Query{Stream: stream})
	if err != nil {
		t.Fatalf("read spine: %v", err)
	}
	var out []spine.Event
	for _, e := range events {
		if e.Type == "approval.decision" {
			out = append(out, e)
		}
	}
	return out
}

// lastApprovalDecision returns the payload of the final authorization decision on the
// record, failing when there is none: a gate that decided and wrote nothing down is the
// failure these tests exist to catch.
func lastApprovalDecision(t *testing.T, log spine.Log, stream string) map[string]any {
	t.Helper()
	events := approvalEvents(t, log, stream)
	if len(events) == 0 {
		t.Fatal("the gate reached a decision and recorded nothing")
	}
	return events[len(events)-1].Payload
}

package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/llm/llmtest"
	"github.com/ionalpha/flynn/resource"
)

// TestExportReportsARunWithNoRecord proves exporting a run that was never sealed fails
// with the store's reason rather than writing an empty file. The record is the artifact a
// third party verifies, so a file that exists and carries nothing is worse than no file.
func TestExportReportsARunWithNoRecord(t *testing.T) {
	s, _ := newSlashSession(t, llmtest.NewScripted())
	s.started = true
	s.runID = "run-with-no-record"

	path, err := s.export(context.Background(), "")
	if err == nil {
		t.Fatalf("exported an unsealed run to %q", path)
	}
	if path != "" {
		t.Fatalf("a failed export named a path: %q", path)
	}
}

// TestForkReportsAMissingRun proves a branch off a run the store does not hold is
// reported, and that the session stays on the run it was on. Switching first and failing
// after would leave the session recording onto an id nothing opened.
func TestForkReportsAMissingRun(t *testing.T) {
	s, _ := newSlashSession(t, llmtest.NewScripted())
	s.started = true
	s.runID = "run-that-is-not-there"

	forkID, err := s.fork(context.Background())
	if err == nil {
		t.Fatalf("forked a run the store does not hold, as %q", forkID)
	}
	if s.runID != "run-that-is-not-there" {
		t.Fatalf("the failed fork moved the session onto %q", s.runID)
	}
}

// TestForkBranchesTheConversation drives a turn, branches it, and proves the fork is a
// new run that records on its own chain while the parent keeps its id and its history.
// The annotation is what makes the lineage readable from the fork's resource alone.
func TestForkBranchesTheConversation(t *testing.T) {
	ctx := context.Background()
	s, buf := newSlashSession(t, llmtest.NewScripted(llmtest.SayText("done")))
	if _, err := s.runTurn(ctx, "do the thing", nil, nil); err != nil {
		t.Fatalf("turn: %v\n%s", err, buf.String())
	}
	parentID := s.runID

	forkID, err := s.fork(ctx)
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	if forkID == parentID || forkID == "" {
		t.Fatalf("fork = %q, want a new run id beside %q", forkID, parentID)
	}
	if s.runID != forkID {
		t.Fatalf("the session stayed on %q instead of the fork", s.runID)
	}
	if s.lastSeq != 0 {
		t.Fatalf("the fork opened at sequence %d rather than the start of its own stream", s.lastSeq)
	}
}

// TestWithForkParentLeavesTheParentAlone pins that recording the lineage copies the
// parent's annotations rather than writing into them: the two runs are separate records,
// and a fork must not edit the run it branched from.
func TestWithForkParentLeavesTheParentAlone(t *testing.T) {
	parent := map[string]string{"flynn/kept": "yes"}

	forked := withForkParent(parent, "parent-run")
	if forked[forkParentAnnotation] != "parent-run" {
		t.Fatalf("the fork does not name its parent: %v", forked)
	}
	if forked["flynn/kept"] != "yes" {
		t.Fatalf("the parent's own annotations were dropped: %v", forked)
	}
	if _, ok := parent[forkParentAnnotation]; ok {
		t.Fatalf("the parent's annotations were written into: %v", parent)
	}
}

// TestSealReportsWhatIsMissing proves the two states a seal cannot happen in are reported
// as such rather than failing the session: nothing has run yet, or the instance has no
// signing key. Both leave the session usable, which is why they are errors returned to
// the loop and not to the caller of the process.
func TestSealReportsWhatIsMissing(t *testing.T) {
	ctx := context.Background()

	s, _ := newSlashSession(t, llmtest.NewScripted())
	if err := s.seal(ctx); err == nil || !strings.Contains(err.Error(), "nothing to seal") {
		t.Fatalf("seal on a session that has not run = %v", err)
	}

	s.started, s.signer = true, nil
	if err := s.seal(ctx); err == nil || !strings.Contains(err.Error(), "signing key") {
		t.Fatalf("seal with no signing key = %v", err)
	}
}

// TestTurnReportsARunTheStoreLost proves a turn on a run that is no longer in the store
// fails with the store's reason rather than opening a second run under the same session.
// The session's id is what its record is written under, so continuing past this would
// record the rest of the conversation somewhere nobody is looking.
func TestTurnReportsARunTheStoreLost(t *testing.T) {
	s, buf := newSlashSession(t, llmtest.NewScripted(llmtest.SayText("done")))
	s.started = true
	s.runID = "a-run-that-is-not-there"

	if _, err := s.runTurn(context.Background(), "carry on", nil, nil); err == nil {
		t.Fatalf("a turn ran on a run the store does not hold:\n%s", buf.String())
	}
	if s.runID != "a-run-that-is-not-there" {
		t.Fatalf("the failed turn moved the session onto %q", s.runID)
	}
}

// TestTurnReportsAStoreThatHasGoneAway proves the first turn of a session whose durable
// store is no longer usable ends with that reason rather than a panic or a turn that runs
// unrecorded. Every turn is assembled against the store, so this is the failure the
// session has to survive reporting.
func TestTurnReportsAStoreThatHasGoneAway(t *testing.T) {
	s, buf := newSlashSession(t, llmtest.NewScripted(llmtest.SayText("done")))
	if err := s.store.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := s.runTurn(context.Background(), "do the thing", nil, nil); err == nil {
		t.Fatalf("a turn ran against a closed store:\n%s", buf.String())
	}
	if s.started {
		t.Fatal("a turn that never ran left the session marked as started")
	}
}

// TestReplayReportsARecordItCannotRead proves /replay surfaces a record it cannot read
// rather than printing an empty transcript, which would read as a run that did nothing.
func TestReplayReportsARecordItCannotRead(t *testing.T) {
	s, _ := newSlashSession(t, llmtest.NewScripted())
	s.started = true
	s.runID = "some-run"
	if err := s.store.Close(); err != nil {
		t.Fatal(err)
	}

	handled, err := s.replCommand(context.Background(), "/replay")
	if !handled {
		t.Fatal("/replay reached the model as a prompt")
	}
	if err == nil {
		t.Fatal("/replay reported nothing when it could not read the record")
	}
}

// TestTurnReportsAnUnreadableCheckpoint proves a run whose recorded state cannot be
// decoded stops the turn with that reason. Continuing would reopen the conversation from
// a checkpoint nobody read, so the model would answer without the exchange it is in.
func TestTurnReportsAnUnreadableCheckpoint(t *testing.T) {
	ctx := context.Background()
	s, buf := newSlashSession(t, llmtest.NewScripted(llmtest.SayText("done")))
	spec, err := json.Marshal(goal.Spec{Objective: "do the thing", StopCondition: defaultStopCondition})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.Resources(s.reg).Put(ctx, resource.Resource{
		APIVersion: goal.GroupVersion, Kind: goal.Kind, Name: "damaged-run",
		Spec: spec, Status: []byte(`{"phase":404}`),
	}); err != nil {
		t.Fatal(err)
	}
	s.started, s.runID = true, "damaged-run"

	if _, err := s.runTurn(ctx, "carry on", nil, nil); err == nil {
		t.Fatalf("a turn continued from a checkpoint nothing could read:\n%s", buf.String())
	}
}

// TestExternalTurnReportsAHarnessThatBuildsNoLoop proves the external half of assembling a
// turn surfaces a harness that cannot produce one. The two halves are assembled from the
// same pieces and differ only in who drives the loop, so the failure has to arrive the
// same way: as the turn's error, with the session left where it was.
func TestExternalTurnReportsAHarnessThatBuildsNoLoop(t *testing.T) {
	s, buf := newSlashSession(t, llmtest.NewScripted())
	s.ext = &externAgent{model: "sonnet", driver: &plainDriver{stubHarnessDriver{name: "claude"}}}

	if _, err := s.runTurn(context.Background(), "do the thing", nil, nil); err == nil {
		t.Fatalf("an episode ran on a harness that builds no loop:\n%s", buf.String())
	}
	if s.started {
		t.Fatal("a turn that never ran left the session marked as started")
	}
}

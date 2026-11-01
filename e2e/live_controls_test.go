package e2e

import (
	"strings"
	"testing"
	"time"
)

// The operator's controls over a run that is driven by a real model: an approval nobody is
// there to give, a kill and a steer from a second process, a resume after a hard kill, and
// a spend cap. The model's choices vary between runs, so each scenario holds the model to a
// task long enough to still be going when the control arrives, and asserts on the record
// and on the host.

// manyFiles is an objective that keeps a model busy across many turns: one file per call
// forces a turn each, where a model asked for several at once would batch them.
const manyFiles = "Create the 20 files part01.txt through part20.txt in the current directory, each " +
	"containing its own number. Use the write tool, one call per file, and wait for each result " +
	"before writing the next. Do not use any other tool."

// waitForLiveRun is [waitForRun] for a goal started with liveStart: it returns the id of
// the run whose objective contains objective, and if the goal exits before that run is
// listed it fails with the goal's own output, which is the only place the reason is.
// The output is read only after the process has exited, since it is still being written
// until then.
func waitForLiveRun(t *testing.T, in *instance, goal *running, objective string) string {
	t.Helper()
	for range pollAttempts {
		for _, line := range scanLines(in.run("runs").stdout) {
			if strings.Contains(line, objective) {
				return strings.Fields(line)[0]
			}
		}
		select {
		case err := <-goal.done:
			goal.done <- err
			t.Fatalf("the goal exited (%v) before a run for %q was listed\nstdout:\n%s\nstderr:\n%s", err, objective, goal.out.String(), goal.errb.String())
		case <-time.After(100 * time.Millisecond):
		}
	}
	t.Fatalf("no run for %q appeared", objective)
	return ""
}

// waitForWorkfile polls the workspace until name exists. It fails at once if the goal exits
// without having written it, since a model that answers without a tool call would
// otherwise be waited on for the whole bound.
func waitForWorkfile(t *testing.T, in *instance, goal *running, name string, within time.Duration) {
	t.Helper()
	deadline := time.After(within)
	for {
		if _, err := in.workfile(name); err == nil {
			return
		}
		select {
		case err := <-goal.done:
			goal.done <- err
			if _, statErr := in.workfile(name); statErr == nil {
				return
			}
			t.Fatalf("the goal exited (%v) without writing %s; the model did not make the tool call this scenario needs\nstdout:\n%s", err, name, goal.out.String())
		case <-deadline:
			t.Fatalf("%s never appeared within %s", name, within)
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// TestLiveApprovalRequiredWithNobodyToAskIsRefused: with a required approval and no one to
// give it, the model's write is refused at the dispatch waist and recorded as refused, and
// the file is not created.
func TestLiveApprovalRequiredWithNobodyToAskIsRefused(t *testing.T) {
	in := liveInstance(t, liveModel(t))
	res := in.liveGoal(
		"Create the file approved.txt containing the word ok, using the write tool. "+
			"If the tool refuses, say so and stop; do not try another way.",
		"-require-approval", "write",
	)
	rec := in.sealedRun(res)

	if len(rec.calls("write")) == 0 {
		t.Fatalf("the model never called the write tool, so the approval gate was not exercised; tools called: %v", callNames(rec))
	}
	if !containsString(rec.rejections(), "approval_required") {
		t.Errorf("the record shows no approval_required refusal; refusals: %v", rec.rejections())
	}
	if _, err := in.workfile("approved.txt"); err == nil {
		t.Error("the write ran although it required approval and nobody gave it")
	}
}

// TestLiveKillAndSteerFromASecondProcess steers a running goal and then kills it, each from
// its own invocation of the binary. The steer is accepted while the run is live; the kill
// stops the run at its next action, naming the operator's reason, and leaves the work it
// had not reached undone.
func TestLiveKillAndSteerFromASecondProcess(t *testing.T) {
	in := liveInstance(t, liveModel(t))
	goal := in.liveStart(manyFiles)

	id := waitForLiveRun(t, in, goal, "part01.txt")
	waitForWorkfile(t, in, goal, "part01.txt", 3*time.Minute)

	steer := in.run("steer", id, "Also put the word steered in every file you have not written yet.")
	requireExit(t, steer, 0, "steer")
	requireContains(t, steer.stdout, "steered", "steer confirmation")

	requireExit(t, in.run("kill", id, "enough"), 0, "kill")
	select {
	case err := <-goal.done:
		_ = err
	case <-time.After(3 * time.Minute):
		t.Fatal("the killed goal did not exit")
	}
	requireContains(t, goal.errb.String(), "stopped by the operator: enough", "the killed goal's exit")

	if _, err := in.workfile("part20.txt"); err == nil {
		t.Error("the run reached its last file after it was killed")
	}
	// A steer reaches the record as a judgement of the run's account of finishing, which a
	// killed run never gives, so the record is checked for integrity only.
	verified := in.verify(id)
	requireContains(t, verified.stdout, "integrity:    VERIFIED", "spine verify of the killed run")
}

// TestLiveResumeAfterAHardKill kills the process dead after the model has begun its work,
// then resumes the run from the durable store. The files written before the kill are still
// there, the resumed run finishes the objective, and the record still verifies.
//
// A run killed in the middle of a model call exercises the resume path that re-issues the
// call; the bound on the resume is what turns a hang there into a failure.
func TestLiveResumeAfterAHardKill(t *testing.T) {
	in := liveInstance(t, liveModel(t))
	const objective = "Create the files one.txt, two.txt and three.txt in the current directory, each " +
		"containing its own name. Use the write tool, one call per file, one at a time."
	goal := in.liveStart(objective)

	id := waitForLiveRun(t, in, goal, "one.txt")
	waitForWorkfile(t, in, goal, "one.txt", 3*time.Minute)
	goal.kill(t)

	runs := in.run("runs")
	if !containsPhase(runs.stdout, id, "Running", "Pending", "Stalled") {
		t.Fatalf("the killed run is not in a resumable phase:\n%s", runs.stdout)
	}
	if _, err := in.workfile("one.txt"); err != nil {
		t.Fatalf("the work done before the kill was lost: %v", err)
	}

	in.timeout = 4 * time.Minute
	resumed := in.run("resume", id)
	requireExit(t, resumed, 0, "resume after a hard kill")
	for _, name := range []string{"one.txt", "two.txt", "three.txt"} {
		if _, err := in.workfile(name); err != nil {
			t.Errorf("the resumed run did not finish its objective: %v", err)
		}
	}
	requireExit(t, in.verify(id), 0, "verify the resumed run")
}

// TestLiveSpendCapStopsTheRun gives a task that needs many turns a token ceiling the first
// turn alone exceeds. The run must stop on the cap with a budget reason and a non-zero
// exit, after at most a couple of model turns, whatever the model would have done next.
func TestLiveSpendCapStopsTheRun(t *testing.T) {
	in := liveInstance(t, liveModel(t))
	res := in.liveGoal(manyFiles, "-max-tokens", "50")
	rec := in.loadRecord(in.runID(res))
	if len(rec.calls()) == 0 {
		t.Skip("the model answered in one turn without calling a tool, so the cap had no second turn to stop")
	}
	requireExit(t, res, 1, "goal under a tiny token cap")
	requireContains(t, res.combined(), "budget_exceeded", "the budget stop reason")
	if n := len(rec.calls()); n > 3 {
		t.Errorf("%d tool calls were made under a 50-token cap", n)
	}
	if _, err := in.workfile("part20.txt"); err == nil {
		t.Error("the run finished its whole objective under a 50-token cap")
	}
}

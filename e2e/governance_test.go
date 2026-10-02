package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The controls an operator puts on a run, through the shipped binary: a goal spec's
// terms, a required approval with nobody present to give it, an irreversible action
// declared and undeclared, and a kill issued from a second invocation. Each is the
// documented command line, so a control that parses into something other than a
// control fails here.

// writeGoalSpec writes a goal spec file outside the workspace and returns its path.
func writeGoalSpec(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "terms.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestGoalSpecBreachStopsTheRunNamingTheTerm: a term whose check fails stops the run
// before it reports success. On a host whose sandbox can contain the check, the stop
// names the broken term; on one that cannot, the run stops saying the check could not
// run. Either way it does not finish as though the term held.
func TestGoalSpecBreachStopsTheRunNamingTheTerm(t *testing.T) {
	fake := newFakeOpenAIQueue(t,
		toolCall("call_1", "write", `{"path":"a.txt","content":"x"}`),
		finalText("Done."),
	)
	in := newInstance(t).withModel(fake)
	spec := writeGoalSpec(t, `{
	  "objective": "write a.txt",
	  "invariants": [{"id":"clean-tree","statement":"the tree stays clean","check":"`+failingCheck+`"}]
	}`)

	res := in.run("-no-learn", "goal", "--goal-spec", spec)
	requireExit(t, res, 1, "goal with a breached term")
	requireContains(t, res.stdout, "clean-tree: the tree stays clean", "the terms the run was held to")
	out := res.combined()
	if !strings.Contains(out, "invariant broken: the tree stays clean") && !strings.Contains(out, "could not run") {
		t.Fatalf("the run stopped for a reason other than its term:\n%s", out)
	}
}

// TestGoalSpecTermsThatHoldLetTheRunFinish is the control for the breach: the same
// command line with a term whose check passes converges.
func TestGoalSpecTermsThatHoldLetTheRunFinish(t *testing.T) {
	fake := newFakeOpenAIQueue(t,
		toolCall("call_1", "write", `{"path":"a.txt","content":"x"}`),
		finalText("Done."),
	)
	in := newInstance(t).withModel(fake)
	spec := writeGoalSpec(t, `{
	  "objective": "write a.txt",
	  "invariants": [{"id":"always","statement":"the working directory exists","check":"`+passingCheck+`"}]
	}`)

	res := in.run("-no-learn", "goal", "--goal-spec", spec)
	if strings.Contains(res.combined(), "could not run") {
		t.Skip("this host's sandbox cannot contain a term's check, so a passing term cannot be shown")
	}
	requireExit(t, res, 0, "goal whose term holds")
	if _, err := in.workfile("a.txt"); err != nil {
		t.Fatalf("the run did not do its work: %v", err)
	}
}

// TestRequiredApprovalWithNobodyToAskIsRefused: a goal has no one to prompt, so an
// action that needs a person's authorization is refused rather than taken, and the
// refusal is what the model is told.
func TestRequiredApprovalWithNobodyToAskIsRefused(t *testing.T) {
	fake := newFakeOpenAIQueue(t,
		toolCall("call_1", "write", `{"path":"needs-approval.txt","content":"x"}`),
		finalText("Could not write it."),
	)
	in := newInstance(t).withModel(fake)

	res := in.run("-no-learn", "goal", "--require-approval", "write", "write needs-approval.txt")
	requireContains(t, res.stdout, "approval required (refused, nothing here can prompt): write", "the run's stated controls")
	if _, err := in.workfile("needs-approval.txt"); err == nil {
		t.Fatal("an action that required approval was taken with nobody to approve it")
	}
	var told bool
	for i := range fake.count() {
		for _, m := range fake.request(t, i).Messages {
			if m.Role == "tool" && strings.Contains(m.Content, "approval_required") {
				told = true
			}
		}
	}
	if !told {
		t.Fatal("the model was not told the action needed approval")
	}
}

// TestUndeclaredIrreversibleActionStopsTheRun: an action marked irreversible and not
// declared stops the run with the ask, rather than being taken or handed back as a
// refusal the run might route around. Declared with --allow, the same action is taken.
func TestUndeclaredIrreversibleActionStopsTheRun(t *testing.T) {
	script := func() *fakeOpenAI {
		return newFakeOpenAIQueue(t,
			toolCall("call_1", "shell", `{"command":"`+passingCheck+`"}`),
			finalText("Done."),
		)
	}

	undeclared := newInstance(t).withModel(script())
	res := undeclared.run("-no-learn", "goal", "--irreversible", "shell", "run a command")
	requireExit(t, res, 1, "an undeclared irreversible action")
	requireContains(t, res.combined(), "shell reaches outside the workspace and cannot be undone", "the stop names the action")

	fake := script()
	declared := newInstance(t).withModel(fake)
	res = declared.run("-no-learn", "goal", "--irreversible", "shell", "--allow", "shell", "run a command")
	requireExit(t, res, 0, "a declared irreversible action")
	for i := range fake.count() {
		for _, m := range fake.request(t, i).Messages {
			if m.Role == "tool" && strings.Contains(m.Content, "allowance_required") {
				t.Fatalf("a declared action was refused: %s", m.Content)
			}
		}
	}
}

// TestKillStopsARunningGoalBeforeItsNextAction: `flynn kill`, from a second invocation,
// stops a goal that is mid-turn. The model's reply is held until the kill has taken hold
// (the run's phase reads Stalled), so the write it then asks for is the action the halt
// must refuse: the file never appears, and the goal exits naming the operator's reason.
func TestKillStopsARunningGoalBeforeItsNextAction(t *testing.T) {
	release := make(chan struct{})
	var answered atomic.Bool
	fake := newFakeOpenAIFunc(t, func(oaiRequest, int) oaiReply {
		if answered.CompareAndSwap(false, true) {
			<-release
			return toolCall("call_1", "write", `{"path":"after-kill.txt","content":"x"}`)
		}
		return finalText("Done.")
	})
	in := newInstance(t).withModel(fake)

	const objective = "write a file after a long think"
	goal := in.start("-no-learn", "goal", objective)
	fake.waitForCount(t, 1, 30*time.Second)
	id := waitForRun(t, in, objective)

	requireExit(t, in.run("kill", id, "wrong repository"), 0, "kill")
	waitForPhase(t, in, id, "Stalled")
	close(release)

	select {
	case <-goal.done:
	case <-time.After(60 * time.Second):
		t.Fatal("the killed goal did not exit")
	}
	requireContains(t, goal.errb.String(), "stopped by the operator: wrong repository", "the killed goal's exit")
	if _, err := in.workfile("after-kill.txt"); err == nil {
		t.Fatal("the run took an action after it was killed")
	}
}

// pollAttempts bounds a wait on the binary's state at about 30 seconds, polled every
// 100ms: a count of attempts rather than a deadline, because the suite takes no wall
// clock.
const pollAttempts = 300

// waitForRun polls `flynn runs` until a run with objective appears, returning its id.
func waitForRun(t *testing.T, in *instance, objective string) string {
	t.Helper()
	for range pollAttempts {
		for _, line := range scanLines(in.run("runs").stdout) {
			if strings.Contains(line, objective) {
				return strings.Fields(line)[0]
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("no run for %q appeared", objective)
	return ""
}

// waitForPhase polls `flynn describe goals` until the goal reports phase.
func waitForPhase(t *testing.T, in *instance, id, phase string) {
	t.Helper()
	var last string
	for range pollAttempts {
		last = in.run("describe", "goals", id).stdout
		for _, line := range scanLines(last) {
			if f := strings.Fields(line); len(f) == 2 && f[0] == "PHASE:" && f[1] == phase {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("goal %s never reached phase %s; last describe:\n%s", id, phase, last)
}

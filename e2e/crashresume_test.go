package e2e

import (
	"strings"
	"testing"
	"time"
)

// TestCrashMidRunLeavesStateIntact hard-kills the binary mid-run (a crash:
// TerminateProcess on Windows, SIGKILL elsewhere) and proves the durable state survived
// with no corruption: post-crash commands still work, the run is listed in a resumable
// phase, and the partial artifact the run had already written is still on disk. This is
// the "a crash never corrupts the store" half of the crash/resume guarantee.
func TestCrashMidRunLeavesStateIntact(t *testing.T) {
	// req0 runs a write tool (persisting events + an artifact); req1 (the fold) blocks
	// so the binary is frozen mid-run at a known point for the kill.
	crashFake := newFakeOpenAIQueue(
		t,
		toolCall("w", "write", `{"path":"progress.txt","content":"partial work"}`),
		finalText("unreached"),
	)
	crashFake.blockAt(1)
	in := newInstance(t).withModel(crashFake)

	proc := in.start("-no-learn", "goal", "resumable objective")
	crashFake.waitForCount(t, 2, 20*time.Second)
	time.Sleep(300 * time.Millisecond) // let the write + its events settle to disk
	proc.kill(t)

	runID := in.runID(result{stdout: proc.out.String()})

	// Store readable after a hard kill; the run is listed in a non-terminal phase.
	runs := in.run("runs")
	requireExit(t, runs, 0, "runs after crash")
	requireContains(t, runs.stdout, runID, "crashed run still listed")
	if !containsPhase(runs.stdout, runID, "Running", "Pending", "Stalled") {
		t.Fatalf("crashed run not in a resumable phase:\n%s", runs.stdout)
	}

	// The run is inspectable (its recorded events replay) and the pre-crash artifact
	// survived: the crash lost neither the record nor the work already done.
	insp := in.run("inspect", runID)
	requireExit(t, insp, 0, "inspect crashed run")
	if _, err := in.workfile("progress.txt"); err != nil {
		t.Fatalf("pre-crash artifact lost: %v", err)
	}
}

// TestRestartAfterCrashNoCorruption checks the restart path: after a crash, a fresh
// invocation against the same data dir starts cleanly and converges, with no crash-loop
// and no leftover lock wedging the store.
func TestRestartAfterCrashNoCorruption(t *testing.T) {
	crashFake := newFakeOpenAI(t, finalText("never returned"))
	crashFake.blockAt(0) // block on the very first call, before any convergence
	in := newInstance(t).withModel(crashFake)

	proc := in.start("-no-learn", "goal", "will be crashed immediately")
	crashFake.waitForCount(t, 1, 20*time.Second)
	proc.kill(t)

	// A brand-new run against the same data dir must start and converge cleanly.
	freshFake := newFakeOpenAI(t, finalText("fresh run ok"))
	in.withModel(freshFake)
	res := in.run("-no-learn", "goal", "a healthy run after the crash")
	requireExit(t, res, 0, "run after crash")
	requireContains(t, res.combined(), "fresh run ok", "post-crash run converged")
	requireExit(t, in.verify(in.runID(res)), 0, "verify post-crash run")
}

// TestResumeConvergedRunReplays exercises the resume command on a completed run: it
// re-drives from the durable events, re-renders the recorded conversation, and exits
// cleanly without corrupting the run (which still verifies afterward). This is the
// resume path that is known-good today.
//
// NOTE (finding, tracked in COVERAGE.md): resuming a run that was interrupted *mid model
// call* (its last turn never completed) does not re-issue the call; it hangs instead of
// continuing or failing safe. That case is deliberately not asserted here so the suite
// cannot hang; it is recorded as a gap for the run/session layer to fix. Model
// resolution is identical to a fresh goal (both go through provider.ResolveWith with the
// same credential source), so the hang is in the resume/replay continuation, not config.
func TestResumeConvergedRunReplays(t *testing.T) {
	fake := newFakeOpenAIFunc(t, func(_ oaiRequest, _ int) oaiReply { return finalText("TURN-ANSWER") })
	in := newInstance(t).withModel(fake)

	res := in.run("-no-learn", "goal", "a completed objective")
	requireExit(t, res, 0, "goal")
	runID := in.runID(res)
	callsBefore := fake.count()

	resume := in.run("resume", runID)
	requireExit(t, resume, 0, "resume converged run")
	requireContains(t, resume.combined(), "TURN-ANSWER", "resume re-renders the recorded turn")

	// Resume of a converged run replays from events; it must not corrupt the record.
	requireExit(t, in.verify(runID), 0, "verify after resume")
	// The run stays a single record: replay did not fork or duplicate it.
	if lines := runLinesFor(in.run("runs").stdout, runID); lines != 1 {
		t.Fatalf("expected exactly one run row for %s after resume, got %d", runID, lines)
	}
	_ = callsBefore
}

// containsPhase reports whether the runs table lists runID in any of the given phases.
func containsPhase(runsOutput, runID string, phases ...string) bool {
	for _, line := range scanLines(runsOutput) {
		if !strings.Contains(line, runID) {
			continue
		}
		for _, p := range phases {
			if strings.Contains(line, p) {
				return true
			}
		}
	}
	return false
}

// runLinesFor counts the run-table rows mentioning runID.
func runLinesFor(runsOutput, runID string) int {
	n := 0
	for _, line := range scanLines(runsOutput) {
		if strings.Contains(line, runID) {
			n++
		}
	}
	return n
}

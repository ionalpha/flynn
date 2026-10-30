package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRequireProofRefusesACompletionClaimOverAnUnprovenItem is the whole point of a
// ledger: the model reports success, the item's own declared check does not back it, and
// the run stops rather than reporting a success its record cannot show. The stall names the
// item and why, so the failure is actionable rather than a bare refusal.
func TestRequireProofRefusesACompletionClaimOverAnUnprovenItem(t *testing.T) {
	fake := newFakeOpenAI(t, finalText("All done, everything works.")).planWith(failingPlan)
	in := newInstance(t).withModel(fake)

	res := in.run("-no-learn", "-require-proof", "goal", "state the answer")
	if res.code == 0 {
		t.Fatalf("a completion claim over an unproven item exited 0:\n%s", res.stdout)
	}
	requireContains(t, res.combined(), "unproven", "the stall reason")

	// The reason has to say which of the failure modes this is. Two are legitimate here
	// and they need opposite responses: on a host that can contain semi-trusted work the
	// check runs and fails, and on one that cannot it is refused before it runs. What the
	// run must never do is report either as success.
	reason := res.combined()
	if !strings.Contains(reason, "its check ran and did not pass") &&
		!strings.Contains(reason, "its check could not be run") {
		t.Fatalf("the stall did not say why the item is unproven:\n%s", reason)
	}
}

// TestWithoutRequireProofTheClaimStillStands: the refusal is what changed the outcome, not
// the failing check on its own. The same run without the flag converges, so the flag is a
// decision an operator makes rather than a behaviour that arrived with planning.
func TestWithoutRequireProofTheClaimStillStands(t *testing.T) {
	fake := newFakeOpenAI(t, finalText("All done, everything works.")).planWith(failingPlan)
	in := newInstance(t).withModel(fake)

	res := in.run("-no-learn", "goal", "state the answer")
	requireExit(t, res, 0, "goal without --require-proof")
	requireContains(t, res.stdout, "All done, everything works.", "goal output")
}

// TestTheProducerRunsWithoutTheRefusal: planning always runs the item's check and records
// the verdict, whether or not the refusal is on. That is what the staged rollout depends
// on, and it is why the run below still converges while leaving a verifiable record behind.
func TestTheProducerRunsWithoutTheRefusal(t *testing.T) {
	fake := newFakeOpenAI(t, finalText("The answer is 42."))
	in := newInstance(t).withModel(fake)

	res := in.run("-no-learn", "goal", "state the answer")
	requireExit(t, res, 0, "goal")
	requireContains(t, res.stdout, "The answer is 42.", "goal output")

	ver := in.verify(in.runID(res))
	requireExit(t, ver, 0, "spine verify")
	requireContains(t, ver.stdout, "VERIFIED", "verify integrity")
	requireContains(t, ver.stdout, "governance:", "verify report")
}

// TestAFileStandingInForTheCheckIsRefusedAsTampering: the plan's check is `cd .`, which
// passes on every host, and the workspace holds a file named `cd`. A shell resolves a bare
// command through the working directory on some hosts, so that file would answer to the
// check in place of the program and the check would measure what the agent wrote. The run
// has to refuse it, by name and as tampering, rather than run it or report the item proven.
// Without the refusal wired into the CLI's worker the check runs and passes, or on a host
// that cannot confine it is reported as merely unrunnable; neither says tampering.
func TestAFileStandingInForTheCheckIsRefusedAsTampering(t *testing.T) {
	fake := newFakeOpenAI(t, finalText("All done, everything works."))
	in := newInstance(t).withModel(fake)
	if err := os.WriteFile(filepath.Join(in.workspace, "cd"), []byte("exit 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res := in.run("-no-learn", "-require-proof", "goal", "state the answer")
	if res.code == 0 {
		t.Fatalf("a run whose check was answered by a workspace file exited 0:\n%s", res.stdout)
	}
	requireContains(t, res.combined(), "tampering", "the stall names the stand-in as tampering")
	requireContains(t, res.combined(), "a file named cd", "the stall names the file")
}

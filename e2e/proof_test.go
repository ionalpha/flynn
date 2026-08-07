package e2e

import "testing"

// TestRequireProofConvergesWhenTheLedgerIsSettled: with --require-proof the run is held to
// its own plan, and a plan whose item's check actually passes converges exactly as before.
// The flag is a refusal, not a tax on honest runs.
func TestRequireProofConvergesWhenTheLedgerIsSettled(t *testing.T) {
	fake := newFakeOpenAI(t, finalText("The answer is 42."))
	in := newInstance(t).withModel(fake)

	res := in.run("-no-learn", "-require-proof", "goal", "state the answer")
	requireExit(t, res, 0, "goal --require-proof")
	requireContains(t, res.stdout, "The answer is 42.", "goal output")

	ver := in.verify(in.runID(res))
	requireExit(t, ver, 0, "spine verify")
	requireContains(t, ver.stdout, "VERIFIED", "verify integrity")
}

// TestRequireProofRefusesACompletionClaimOverAnUnprovenItem is the whole point of a ledger:
// the model reports success, the item's own declared check says otherwise, and the run
// stops rather than reporting a success its record cannot show. The stall names the item
// and why, so the failure is actionable rather than a bare refusal.
func TestRequireProofRefusesACompletionClaimOverAnUnprovenItem(t *testing.T) {
	fake := newFakeOpenAI(t, finalText("All done, everything works."))
	fake.planWith(failingPlan)
	in := newInstance(t).withModel(fake)

	res := in.run("-no-learn", "-require-proof", "goal", "state the answer")
	if res.code == 0 {
		t.Fatalf("a completion claim over an unproven item exited 0:\n%s", res.stdout)
	}
	requireContains(t, res.combined(), "unproven", "the stall reason")
	requireContains(t, res.combined(), "no recorded passing verification", "why the item is unproven")

	// The same run without the refusal converges, which is what makes the flag the thing
	// that changed the outcome rather than the failing check on its own.
	fake2 := newFakeOpenAI(t, finalText("All done, everything works."))
	fake2.planWith(failingPlan)
	in2 := newInstance(t).withModel(fake2)
	requireExit(t, in2.run("-no-learn", "goal", "state the answer"), 0, "goal without --require-proof")
}

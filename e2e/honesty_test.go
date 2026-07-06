package e2e

import (
	"testing"
)

// TestHonestyUngroundedClaimRejected proves the headline honesty property through the
// shipped binary: a run's success is grounded in an independent check, not in the
// model's narrative. The model asserts everything works, but the bound -verify check
// fails; the record must mark the success NOT GROUNDED and `spine verify` must reject it.
// A downstream consumer gating on the record therefore cannot be fooled by a confident
// but false completion claim, whatever the model said.
func TestHonestyUngroundedClaimRejected(t *testing.T) {
	fake := newFakeOpenAI(t, finalText("All done. Everything works and the task is complete."))
	in := newInstance(t).withModel(fake)

	res := in.run("-no-learn", "-verify", "exit 1", "goal", "claim success without doing the work")
	requireExit(t, res, 0, "goal completes but flags the ungrounded claim")
	requireContains(t, res.stdout, "not grounded", "goal flags ungrounded success")

	ver := in.verify(in.runID(res))
	if ver.code == 0 {
		t.Fatalf("spine verify accepted an ungrounded success:\n%s", ver.stdout)
	}
	requireContains(t, ver.stdout, "NOT GROUNDED", "verify ground-truth tier")
	// Integrity and governance are still intact: the record faithfully captures a real,
	// governed run whose only fault is the ungrounded claim. Honesty is not "the run
	// failed", it is "the claim is not backed".
	requireContains(t, ver.stdout, "integrity:", "verify still reports integrity")
	requireContains(t, ver.stdout, "VERIFIED", "integrity holds")
}

// TestHonestyGroundedClaimAccepted is the matching positive case: the same success
// claim, but with a passing independent check, verifies on every tier. Together the two
// tests show grounding is decided by the check, not by the model's words.
func TestHonestyGroundedClaimAccepted(t *testing.T) {
	fake := newFakeOpenAI(t, finalText("Task complete."))
	in := newInstance(t).withModel(fake)

	res := in.run("-no-learn", "-verify", "exit 0", "goal", "finish and pass the check")
	requireExit(t, res, 0, "goal")

	ver := in.verify(in.runID(res))
	requireExit(t, ver, 0, "spine verify grounded run")
	requireContains(t, ver.stdout, "GROUNDED", "verify ground-truth grounded")
}

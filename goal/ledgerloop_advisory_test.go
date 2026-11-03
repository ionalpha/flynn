package goal

// A run that does not require proof treats its ledger as a record, not a stop: an item
// whose check keeps failing the same way, or whose check names a program the host does
// not have, is left unproven and the run moves to the next item. A run that requires
// proof stops on the same item, and a repeated refusal with no check behind it stops
// either kind of run.

import (
	"strings"
	"testing"
)

// runAdvisory drives rounds of build, verify and observe against a planned two-item
// ledger whose verifier returns v every time, and returns the status and the items the
// verifier was asked about, in order.
func runAdvisory(t *testing.T, v ItemVerdict, rounds int, opts ...Option) (Status, []string, []LedgerItem) {
	t.Helper()
	ev := &fakeEvidence{}
	ver := &fakeVerifier{verdicts: []ItemVerdict{v}}
	h, w := producerHarness(t, ver, ev, opts...)
	ledger := twoItemLedger(t)
	ref := h.plannedGoal(t, "advisory", ledger)
	for range rounds {
		h.reconcile(t, ref)
		if _, err := w.ProcessOnce(h.ctx); err != nil {
			t.Fatalf("build step: %v", err)
		}
		h.reconcile(t, ref)
		if _, err := w.ProcessOnce(h.ctx); err != nil {
			t.Fatalf("verify step: %v", err)
		}
		h.reconcile(t, ref)
	}
	return h.status(t, ref), ver.asked, ledger
}

// failingCheck is a check that ran and failed with ordinary output: the shape of a check
// the model wrote wrong, which no amount of work will make pass.
var failingCheck = ItemVerdict{Executed: true, ExitCode: 1, Output: "no such line", Detail: "exit 1: no such line"}

// TestAnAdvisoryRunMovesPastAnItemWhoseCheckKeepsFailing is the field failure: a check
// that can never pass ended a run that was told its ledger was only a record. The item is
// left unproven with the failure that gave it up, and the next item's check runs.
func TestAnAdvisoryRunMovesPastAnItemWhoseCheckKeepsFailing(t *testing.T) {
	st, asked, ledger := runAdvisory(t, failingCheck, VerdictRepeatLimit+1)

	if st.Phase == PhaseStalled {
		t.Fatalf("an advisory run stopped on its own check: %s", st.Message)
	}
	first := st.Ledger[0]
	if !first.Abandoned || first.Proven {
		t.Fatalf("first item = %+v, want it left unproven and marked abandoned", first)
	}
	if !strings.Contains(first.AbandonedReason, "no such line") {
		t.Fatalf("abandon reason %q does not say what the check reported", first.AbandonedReason)
	}
	if got := asked[len(asked)-1]; got != ledger[1].ID {
		t.Fatalf("last check run was for %q, want the next item %q", got, ledger[1].ID)
	}
	if st.VerdictRepeat > 1 {
		t.Fatalf("repeat count %d carried over from the abandoned item onto the next", st.VerdictRepeat)
	}
}

// TestAnAdvisoryRunMovesPastAnItemWhoseProgramIsMissing: the same promise for a check
// that names a program this host does not have.
func TestAnAdvisoryRunMovesPastAnItemWhoseProgramIsMissing(t *testing.T) {
	missing := ItemVerdict{
		Executed: true, ExitCode: 9009,
		Output: "'test' is not recognized as an internal or external command,\noperable program or batch file.",
		Detail: "exit 9009: 'test' is not recognized as an internal or external command",
	}
	st, _, _ := runAdvisory(t, missing, EnvironmentRepeatLimit)

	if st.Phase == PhaseStalled {
		t.Fatalf("an advisory run stopped on a missing program: %s", st.Message)
	}
	if !st.Ledger[0].Abandoned || !strings.Contains(st.Ledger[0].AbandonedReason, `"test"`) {
		t.Fatalf("first item = %+v, want it abandoned naming the missing program", st.Ledger[0])
	}
	if st.MissingCommand != "" || st.MissingRepeat != 0 {
		t.Fatalf("missing-program count %q x%d survived the abandon", st.MissingCommand, st.MissingRepeat)
	}
}

// TestAnAdvisoryRunWithEveryCheckFailingKeepsBuilding: once every item is abandoned there
// is nothing left to check, so the run builds toward its answer rather than stalling or
// dispatching a verification for no item.
func TestAnAdvisoryRunWithEveryCheckFailingKeepsBuilding(t *testing.T) {
	st, _, _ := runAdvisory(t, failingCheck, 2*VerdictRepeatLimit)

	if st.Phase == PhaseStalled {
		t.Fatalf("stalled with every item abandoned: %s", st.Message)
	}
	if got := len(st.Abandoned()); got != 2 {
		t.Fatalf("abandoned %d items, want both", got)
	}
	if _, ok := st.CurrentItem(twoItemLedger(t)); ok {
		t.Fatal("a current item remained with every item abandoned")
	}
}

// TestARunThatRequiresProofStillStopsOnTheItem is the control: with proof required the
// unproven item is the reason to stop, so the run stalls naming the failure and nothing is
// marked abandoned.
func TestARunThatRequiresProofStillStopsOnTheItem(t *testing.T) {
	st, _, _ := runAdvisory(t, failingCheck, VerdictRepeatLimit, WithLedgerConvergence())

	if st.Phase != PhaseStalled {
		t.Fatalf("phase = %q, want a run that requires proof stopped on its failing item", st.Phase)
	}
	if reason := stalledReason(st); reason != "NotConverging" {
		t.Fatalf("stalled reason = %q, want NotConverging (message %q)", reason, st.Message)
	}
	if len(st.Abandoned()) != 0 {
		t.Fatalf("a run that requires proof abandoned %d items", len(st.Abandoned()))
	}
}

// TestARepeatedRefusalWithNoCheckBehindItStillStops: moving past an item is about a
// failing check. An evaluator saying the same thing about the whole goal, with no check
// failure in it, is the non-convergence the guard was built for, advisory or not.
func TestARepeatedRefusalWithNoCheckBehindItStillStops(t *testing.T) {
	ev := &fakeEvidence{}
	// The check could not be run, so the cycle carries no executed failure.
	ver := &fakeVerifier{verdicts: []ItemVerdict{{Detail: "no sandbox on this host"}}}
	h := newHarness(t, refusing{"the deploy target does not exist"}, WithLedgerGate(ev, newGate(t, RequireExecuted())))
	w := NewWorker(h.store, h.jobs, h.clk, &fakeExec{}, WithItemVerification(ver, ev))
	ref := h.plannedGoal(t, "refused", twoItemLedger(t))
	for range VerdictRepeatLimit {
		h.reconcile(t, ref)
		if _, err := w.ProcessOnce(h.ctx); err != nil {
			t.Fatalf("build step: %v", err)
		}
		h.reconcile(t, ref)
		if _, err := w.ProcessOnce(h.ctx); err != nil {
			t.Fatalf("verify step: %v", err)
		}
		h.reconcile(t, ref)
	}

	st := h.status(t, ref)
	if reason := stalledReason(st); reason != "NotConverging" {
		t.Fatalf("stalled reason = %q, want NotConverging (phase %q, message %q)", reason, st.Phase, st.Message)
	}
	if len(st.Abandoned()) != 0 {
		t.Fatalf("abandoned %d items on a refusal that had no failing check in it", len(st.Abandoned()))
	}
}

// TestAnAbandonMarkSurvivesTheDispatchWrite: the reconciler sets the mark and a worker's
// copy written in the same window has never seen it. The merge carries it, and reports
// that it did, so the caller does not restore the abandoned item's feedback.
func TestAnAbandonMarkSurvivesTheDispatchWrite(t *testing.T) {
	ledger := twoItemLedger(t)
	mine := []LedgerState{{ID: ledger[0].ID, Abandoned: true, AbandonedReason: "kept failing"}, {ID: ledger[1].ID}}
	fresh := []LedgerState{{ID: ledger[0].ID}, {ID: ledger[1].ID}}

	merged, added := mergeLedger(mine, fresh)
	if !added {
		t.Fatal("the merge did not report carrying an abandon mark the record lacked")
	}
	if !merged[0].Abandoned || merged[0].AbandonedReason != "kept failing" {
		t.Fatalf("merged = %+v, want the abandon mark and its reason kept", merged)
	}

	// A proof on the record outranks a stale abandon mark: an item proven is proven.
	proven := []LedgerState{{ID: ledger[0].ID, Proven: true, Evidence: "1"}, {ID: ledger[1].ID}}
	merged, _ = mergeLedger(mine, proven)
	if !merged[0].Proven || merged[0].Abandoned {
		t.Fatalf("merged = %+v, want the record's proof kept over an abandon mark", merged)
	}
}

package goal

// The build-and-verify alternation. Exactly one check runs per build step, a goal with
// unproven items does not converge however loudly it claims to, and a completion claim
// is verified before it is judged. A ledger with nothing left unproven goes back to
// building rather than dispatching a verification that would find no item.

import (
	"errors"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/resource"
)

// TestLedgerLoopAlternatesBuildingAndVerifying: exactly one check runs per build step, not
// one per reconcile tick, and a verification is not charged to the build budget. A goal
// that proves its items must not get half the budget of one that merely claims them.
func TestLedgerLoopAlternatesBuildingAndVerifying(t *testing.T) {
	ev := &fakeEvidence{}
	h := newHarness(t, neverMet{}, WithLedgerGate(ev, newGate(t, RequireExecuted())))
	ledger := twoItemLedger(t)
	ref := h.plannedGoal(t, "loop", ledger)

	h.reconcile(t, ref) // finalizer + first dispatch
	h.completeJob(t, StepJobKind)
	h.reconcile(t, ref)

	st := h.status(t, ref)
	if !st.VerifyPending {
		t.Fatal("a completed build step did not leave the item's check pending")
	}
	if st.Steps != 1 {
		t.Fatalf("steps = %d after one build step, want 1", st.Steps)
	}
	h.completeJob(t, VerifyJobKind) // asserts the dispatch was the verification

	// A passing executed verdict for the first item lands on the record between the two
	// reconciles, exactly as the worker would have written it.
	if _, err := ev.Record(h.ctx, resource.Resource{}, ledger[0].ID, ItemVerdict{Passed: true, Executed: true}); err != nil {
		t.Fatal(err)
	}
	h.reconcile(t, ref)

	st = h.status(t, ref)
	if st.VerifyPending {
		t.Fatal("an observed verification did not clear the pending mark")
	}
	if st.Steps != 1 {
		t.Fatalf("steps = %d, want the verification not to be charged to the build budget", st.Steps)
	}
	if !st.Ledger[0].Proven {
		t.Fatal("the settling pass did not prove an item the record backs")
	}
	if st.Ledger[0].Evidence != "1" {
		t.Fatalf("item evidence = %q, want the recorded verification's ref", st.Ledger[0].Evidence)
	}
	h.completeJob(t, StepJobKind) // and the loop is back to building
}

// TestGoalDoesNotConvergeWithUnprovenItems: the line the whole task turns on. A model
// reporting completion over an unproven ledger settles as stalled naming each item and
// why, rather than converging on its own say-so.
func TestGoalDoesNotConvergeWithUnprovenItems(t *testing.T) {
	ev := &fakeEvidence{}
	h := newHarness(t, alwaysMet{}, WithLedgerGate(ev, newGate(t, RequireExecuted())), WithLedgerConvergence())
	ledger := twoItemLedger(t)
	ref := h.plannedGoal(t, "claims", ledger)

	h.reconcile(t, ref)

	st := h.status(t, ref)
	if st.Phase != PhaseStalled {
		t.Fatalf("phase = %q, want the completion claim refused: %s", st.Phase, st.Message)
	}
	if !hasCond(st, CondStalled, "True") {
		t.Fatal("the refusal did not set the Stalled condition")
	}
	if !strings.Contains(st.Message, ledger[0].ID) || !strings.Contains(st.Message, ledger[1].ID) {
		t.Fatalf("the stall did not name the unproven items: %q", st.Message)
	}
	if !strings.Contains(st.Message, "no recorded passing verification") {
		t.Fatalf("the stall did not say why the items are unproven: %q", st.Message)
	}
}

// TestACompletionClaimIsVerifiedBeforeItIsJudged: a claim made straight after a build step
// has not been tested yet, so the current item's check runs before the claim is ruled on.
// Without this the run would stall on an item its own check would have proven.
func TestACompletionClaimIsVerifiedBeforeItIsJudged(t *testing.T) {
	ev := &fakeEvidence{}
	h := newHarness(t, alwaysMet{}, WithLedgerGate(ev, newGate(t, RequireExecuted())), WithLedgerConvergence())
	ledger, err := AppendItems(nil, LedgerItem{Item: "the only item", Verify: "true"})
	if err != nil {
		t.Fatal(err)
	}
	ref := h.plannedGoal(t, "one", ledger)
	h.setStatus(t, ref, func(st *Status) { st.VerifyPending = true })

	h.reconcile(t, ref)
	if st := h.status(t, ref); st.Phase == PhaseStalled {
		t.Fatalf("the claim was judged before its check ran: %q", st.Message)
	}
	h.completeJob(t, VerifyJobKind)
	if _, err := ev.Record(h.ctx, resource.Resource{}, ledger[0].ID, ItemVerdict{Passed: true, Executed: true}); err != nil {
		t.Fatal(err)
	}

	h.reconcile(t, ref)
	st := h.status(t, ref)
	if st.Phase != PhaseConverged {
		t.Fatalf("phase = %q, want converged once the ledger settled: %s", st.Phase, st.Message)
	}
	if !st.LedgerSettled() {
		t.Fatal("the goal converged without a settled ledger")
	}
}

// TestUnplannedGoalIsUntouchedByTheLedgerGate: a goal that never planned has no ledger to
// settle, and LedgerSettled is false for an empty one, so without the guard the gate would
// make such a goal unable to converge at all.
func TestUnplannedGoalIsUntouchedByTheLedgerGate(t *testing.T) {
	ev := &fakeEvidence{readErr: errors.New("the record must not even be read")}
	h := newHarness(t, alwaysMet{}, WithLedgerGate(ev, newGate(t, RequireExecuted())), WithLedgerConvergence())
	ref := h.createGoal(t, "bare", Spec{Objective: "o", StopCondition: "c"})

	h.reconcile(t, ref)

	st := h.status(t, ref)
	if st.Phase != PhaseConverged {
		t.Fatalf("phase = %q, want an unplanned goal to converge exactly as before: %s", st.Phase, st.Message)
	}
}

// TestLedgerGateNeedsBothHalves: half a wiring is not a degraded mode. A reconciler given
// only one of the record and the gate leaves the loop open rather than gating on a
// dependency it does not have.
func TestLedgerGateNeedsBothHalves(t *testing.T) {
	for _, tc := range []struct {
		name string
		opt  Option
	}{
		{"no record", WithLedgerGate(nil, newGate(t))},
		{"no gate", WithLedgerGate(&fakeEvidence{}, nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, alwaysMet{}, tc.opt, WithLedgerConvergence())
			ref := h.plannedGoal(t, "half", twoItemLedger(t))
			h.reconcile(t, ref)
			if st := h.status(t, ref); st.Phase != PhaseConverged {
				t.Fatalf("phase = %q, want the loop left open: %s", st.Phase, st.Message)
			}
		})
	}
}

// TestUnreadableRecordRetriesRatherThanStalling: a record that cannot be reached for a
// moment is not a run with no evidence. Reading it as one would stall a healthy goal on
// nothing more than a transient read.
func TestUnreadableRecordRetriesRatherThanStalling(t *testing.T) {
	boom := fault.New(fault.Transient, "evidence_read", "the record is briefly unreachable")
	ev := &fakeEvidence{readErr: boom}
	h := newHarness(t, alwaysMet{}, WithLedgerGate(ev, newGate(t, RequireExecuted())), WithLedgerConvergence())
	ref := h.plannedGoal(t, "unreadable", twoItemLedger(t))

	if _, err := h.gr.Reconcile(h.ctx, ref); !errors.Is(err, boom) {
		t.Fatalf("reconcile error = %v, want the read failure returned for classification", err)
	}
	if st := h.status(t, ref); st.Phase == PhaseStalled {
		t.Fatalf("a transient read failure stalled the goal: %q", st.Message)
	}
}

// TestASettledLedgerBuildsRatherThanVerifyingNothing: a pending check on a ledger with
// nothing left unproven has nothing to run against, so the run goes back to building
// instead of dispatching a verification that would find no item.
func TestASettledLedgerBuildsRatherThanVerifyingNothing(t *testing.T) {
	ledger := twoItemLedger(t)
	h := newHarness(t, neverMet{}, WithLedgerGate(&fakeEvidence{}, newGate(t, RequireExecuted())))
	ref := h.plannedGoal(t, "settled-loop", ledger)
	h.setStatus(t, ref, func(st *Status) {
		st.VerifyPending = true
		for i := range st.Ledger {
			if err := st.MarkProven(st.Ledger[i].ID, "x", testNow); err != nil {
				t.Fatal(err)
			}
		}
	})

	h.reconcile(t, ref)

	h.completeJob(t, StepJobKind) // asserts it was not a verification
	if st := h.status(t, ref); st.VerifyPending {
		t.Fatal("the pending check outlived the ledger it had nothing left to check")
	}
}

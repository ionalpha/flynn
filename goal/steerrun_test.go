package goal

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/reconcile"
	"github.com/ionalpha/flynn/resource"
)

// --- fakes ------------------------------------------------------------------

// fakeJudge accepts the redirects whose ids are in accept, records what it was asked and
// what account it was handed, and can fail instead of answering.
type fakeJudge struct {
	accept  map[string]string
	err     error
	calls   int
	asked   [][]string
	account string
}

func newFakeJudge() *fakeJudge { return &fakeJudge{accept: map[string]string{}} }

func (j *fakeJudge) Acknowledged(_ context.Context, _ resource.Resource, _ Spec, _ Status, outstanding []Steer, account string) ([]Acknowledgement, error) {
	j.calls++
	j.account = account
	ids := make([]string, 0, len(outstanding))
	for _, st := range outstanding {
		ids = append(ids, st.ID)
	}
	j.asked = append(j.asked, ids)
	if j.err != nil {
		return nil, j.err
	}
	var out []Acknowledgement
	for _, st := range outstanding {
		if how, ok := j.accept[st.ID]; ok {
			out = append(out, Acknowledgement{ID: st.ID, How: how})
		}
	}
	return out, nil
}

var _ SteerJudge = (*fakeJudge)(nil)

// --- harness ----------------------------------------------------------------

type steerHarness struct {
	*harness
	judge *fakeJudge
}

func newSteerHarness(t *testing.T, opts ...Option) *steerHarness {
	t.Helper()
	j := newFakeJudge()
	return &steerHarness{harness: newHarness(t, &finishedStop{}, append(opts, WithSteerJudge(j))...), judge: j}
}

// finishedStop reports the stop condition met once the goal has taken a step, giving the
// run's own account of finishing as the reason. That account is what the judge rules on.
type finishedStop struct{ calls int }

func (s *finishedStop) Met(_ context.Context, _ Spec, st Status) (bool, string, error) {
	s.calls++
	return st.Steps >= 1, "wrote the audit trail to the sessions table", nil
}

var _ StopEvaluator = (*finishedStop)(nil)

func steerSpec(steers ...Steer) Spec {
	return Spec{Objective: "add the audit trail", StopCondition: "the trail is written", Steers: steers}
}

// runToClaim reconciles a goal up to and through the step after which its evaluator
// reports the objective achieved.
func (h *steerHarness) runToClaim(t *testing.T, ref reconcile.Ref) {
	t.Helper()
	h.reconcile(t, ref) // finalizer, then dispatch
	h.completeStep(t)
	h.reconcile(t, ref) // observes the step, asks the evaluator, judges the redirects
}

// --- the gate ---------------------------------------------------------------

// TestACompletionClaimIsRefusedWhileARedirectIsUnaddressed is the property the whole
// design rests on. The evaluator says the objective was achieved and the run has said what
// it did; the account says nothing about the operator's redirect, so the goal stops
// un-done with both quoted.
func TestACompletionClaimIsRefusedWhileARedirectIsUnaddressed(t *testing.T) {
	h := newSteerHarness(t)
	ref := h.createGoal(t, "g", steerSpec(wrongTable()))

	h.runToClaim(t, ref)

	st := h.status(t, ref)
	if st.Phase != PhaseStalled {
		t.Fatalf("phase = %q, want Stalled: %+v", st.Phase, st)
	}
	if hasCond(st, CondReady, "True") {
		t.Fatalf("the goal was reported ready over an unaddressed redirect: %+v", st.Conditions)
	}
	for _, c := range st.Conditions {
		if c.Type == CondStalled && c.Reason != "SteerUnaddressed" {
			t.Fatalf("stall reason = %q, want SteerUnaddressed", c.Reason)
		}
	}
	if !strings.Contains(st.Message, wrongTable().Instruction) {
		t.Fatalf("the stall does not say what the operator asked for: %q", st.Message)
	}
	if !strings.Contains(st.Message, "wrote the audit trail to the sessions table") {
		t.Fatalf("the stall does not say what the run claimed instead: %q", st.Message)
	}
	if st.InFlight != nil {
		t.Fatalf("a step was dispatched after the refusal: %+v", st.InFlight)
	}
}

// TestTheJudgeRulesOnTheRunsOwnAccount: the judge is handed the outstanding redirects and
// the sentence the run gave for having finished, which is the statement being judged.
func TestTheJudgeRulesOnTheRunsOwnAccount(t *testing.T) {
	h := newSteerHarness(t)
	ref := h.createGoal(t, "g", steerSpec(wrongTable()))

	h.runToClaim(t, ref)

	if h.judge.calls != 1 {
		t.Fatalf("the judge was asked %d times, want once", h.judge.calls)
	}
	if h.judge.account != "wrote the audit trail to the sessions table" {
		t.Fatalf("the judge was handed %q, want the run's account of finishing", h.judge.account)
	}
	if len(h.judge.asked) != 1 || len(h.judge.asked[0]) != 1 || h.judge.asked[0][0] != wrongTable().ID {
		t.Fatalf("the judge was asked about %v, want the outstanding redirect", h.judge.asked)
	}
}

// TestAnAddressedRedirectLetsTheGoalConverge, and the account that discharged it is on the
// record: whether the operator was answered is a question about that sentence, and a bare
// flag would answer it with "yes" forever.
func TestAnAddressedRedirectLetsTheGoalConverge(t *testing.T) {
	h := newSteerHarness(t)
	h.judge.accept[wrongTable().ID] = "moved the writer onto the events table and re-ran the trail"
	ref := h.createGoal(t, "g", steerSpec(wrongTable()))

	h.runToClaim(t, ref)

	st := h.status(t, ref)
	if st.Phase != PhaseConverged {
		t.Fatalf("phase = %q, want Converged: %+v", st.Phase, st)
	}
	if len(st.Steers) != 1 || !st.Steers[0].Acknowledged {
		t.Fatalf("the discharge was not recorded: %+v", st.Steers)
	}
	if st.Steers[0].Account != "moved the writer onto the events table and re-ran the trail" {
		t.Fatalf("account = %q, want what the judge accepted", st.Steers[0].Account)
	}
	if st.Steers[0].AcknowledgedAt == nil {
		t.Fatalf("the discharge was not stamped: %+v", st.Steers[0])
	}
}

// TestOneAddressedRedirectDoesNotDischargeAnother: a run that answers the redirect it
// found easy and ignores the other has not been answered.
func TestOneAddressedRedirectDoesNotDischargeAnother(t *testing.T) {
	migration := Steer{ID: "leave-migration", Instruction: "leave the migration alone"}
	h := newSteerHarness(t)
	h.judge.accept[migration.ID] = "reverted the migration edit"
	ref := h.createGoal(t, "g", steerSpec(wrongTable(), migration))

	h.runToClaim(t, ref)

	st := h.status(t, ref)
	if st.Phase != PhaseStalled {
		t.Fatalf("phase = %q, want Stalled: %+v", st.Phase, st)
	}
	if !strings.Contains(st.Message, wrongTable().Instruction) || strings.Contains(st.Message, migration.Instruction) {
		t.Fatalf("the stall names the wrong redirect: %q", st.Message)
	}
}

// --- no judge ---------------------------------------------------------------

// TestASteeredRunWithNoJudgeStopsAndCanBeReleased: the obligation could never be
// discharged, so the goal stops naming what is missing rather than burning its step budget
// to arrive at a refusal nobody could have avoided. It is an unwired stall, so wiring a
// judge releases the same goal instead of requiring the run to be recreated.
func TestASteeredRunWithNoJudgeStopsAndCanBeReleased(t *testing.T) {
	h := newHarness(t, &finishedStop{})
	ref := h.createGoal(t, "g", steerSpec(wrongTable()))
	h.reconcile(t, ref)
	h.completeStep(t)
	h.reconcile(t, ref)

	st := h.status(t, ref)
	if st.Phase != PhaseStalled {
		t.Fatalf("phase = %q, want Stalled: %+v", st.Phase, st)
	}
	if !st.Unwired {
		t.Fatalf("the stall was recorded as a fact about the run, not the loop: %+v", st)
	}
	for _, c := range st.Conditions {
		if c.Type == CondStalled && c.Reason != "SteerJudgeMissing" {
			t.Fatalf("stall reason = %q, want SteerJudgeMissing", c.Reason)
		}
	}

	// The same goal, met by a loop that has the judge it needed.
	judge := newFakeJudge()
	judge.accept[wrongTable().ID] = "moved the writer onto the events table"
	wired := NewReconciler(h.store, h.jobs, h.clk, &finishedStop{}, WithSteerJudge(judge))
	if _, err := wired.Reconcile(h.ctx, ref); err != nil {
		t.Fatalf("reconcile with a judge wired: %v", err)
	}
	if st := h.status(t, ref); st.Phase != PhaseConverged {
		t.Fatalf("phase = %q after a judge was wired, want Converged: %+v", st.Phase, st)
	}
}

// TestAJudgeThatCannotAnswerDischargesNothing: "I could not tell" is not "it was
// addressed". Fail-closed either way is the property; which way is the judge's to say, so
// a transient failure retries and an unclassified one settles the goal.
func TestAJudgeThatCannotAnswerDischargesNothing(t *testing.T) {
	t.Run("transient", func(t *testing.T) {
		h := newSteerHarness(t)
		h.judge.err = fault.New(fault.Transient, "judge_unreachable", "the cheap tier is unreachable")
		ref := h.createGoal(t, "g", steerSpec(wrongTable()))

		h.reconcile(t, ref)
		h.completeStep(t)
		_, err := h.gr.Reconcile(h.ctx, ref)
		if err == nil {
			t.Fatal("a judge that could not answer was treated as an acknowledgement")
		}
		if got := fault.Classify(err); got != fault.Transient {
			t.Fatalf("a failed judgement classified %q, want %q so it retries", got, fault.Transient)
		}
		st := h.status(t, ref)
		if st.Phase == PhaseConverged || st.Phase == PhaseStalled {
			t.Fatalf("a transient judge failure settled the goal: %+v", st)
		}
		if len(st.Steers) != 1 || st.Steers[0].Acknowledged {
			t.Fatalf("a redirect was discharged by a failed judge: %+v", st.Steers)
		}

		// It recovers: the same goal converges once the judge answers again.
		h.judge.err = nil
		h.judge.accept[wrongTable().ID] = "moved the writer onto the events table"
		h.reconcile(t, ref)
		if st := h.status(t, ref); st.Phase != PhaseConverged {
			t.Fatalf("the goal did not carry on once the judge came back: %+v", st)
		}
	})

	// An unclassified error is the shape a real caller's failure arrives in, and it
	// classifies Terminal, so the goal settles rather than retrying a judge that is not
	// coming back.
	t.Run("unclassified", func(t *testing.T) {
		h := newSteerHarness(t)
		h.judge.err = errors.New("the cheap tier is unreachable")
		ref := h.createGoal(t, "g", steerSpec(wrongTable()))

		h.reconcile(t, ref)
		h.completeStep(t)
		h.reconcile(t, ref)

		st := h.status(t, ref)
		if st.Phase != PhaseStalled {
			t.Fatalf("a judgement that failed for good left the goal running: %+v", st)
		}
		if !strings.Contains(st.Message, "the cheap tier is unreachable") {
			t.Fatalf("the stall does not say the judgement failed: %q", st.Message)
		}
		if len(st.Steers) != 1 || st.Steers[0].Acknowledged {
			t.Fatalf("a redirect was discharged by a failed judge: %+v", st.Steers)
		}
	})
}

// --- the records themselves --------------------------------------------------

// TestWithdrawingARedirectMidRunIsRefused: the spec is not a surface only the operator can
// reach, so the rule is enforced where every other desired-state record's is, at admission.
func TestWithdrawingARedirectMidRunIsRefused(t *testing.T) {
	h := newSteerHarness(t)
	h.judge.accept[wrongTable().ID] = "moved the writer"
	ref := h.createGoal(t, "g", steerSpec(wrongTable()))
	h.reconcile(t, ref) // takes the redirect on

	h.putSpec(t, ref, steerSpec())
	h.reconcile(t, ref)

	st := h.status(t, ref)
	if st.Phase != PhaseStalled {
		t.Fatalf("phase = %q, want Stalled: %+v", st.Phase, st)
	}
	if !strings.Contains(st.Message, "withdrawn") {
		t.Fatalf("the stall does not name what happened: %q", st.Message)
	}
	if st.Unwired {
		t.Fatal("a withdrawn redirect was recorded as a missing producer")
	}
}

// TestARunNobodySteersIsUntouched: no judge call, no delivery, and the goal converges
// exactly as it did before any of this existed.
func TestARunNobodySteersIsUntouched(t *testing.T) {
	h := newSteerHarness(t)
	ref := h.createGoal(t, "g", steerSpec())

	h.runToClaim(t, ref)

	st := h.status(t, ref)
	if st.Phase != PhaseConverged {
		t.Fatalf("phase = %q, want Converged: %+v", st.Phase, st)
	}
	if h.judge.calls != 0 {
		t.Fatalf("the judge was asked about a run nobody redirected (%d calls)", h.judge.calls)
	}
	if len(st.Steers) != 0 {
		t.Fatalf("an unsteered run carries steer state: %+v", st.Steers)
	}
}

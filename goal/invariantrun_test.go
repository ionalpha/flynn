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

// fakeAuditor rules on the terms it is handed: it reports a breach for every term id in
// breach, records what it was asked and how often, and can fail instead of answering.
type fakeAuditor struct {
	breach map[string]string
	err    error
	calls  int
	asked  [][]string
}

func newFakeAuditor() *fakeAuditor {
	return &fakeAuditor{breach: map[string]string{}}
}

func (a *fakeAuditor) Audit(_ context.Context, _ resource.Resource, _ Spec, _ Status, terms []Invariant) ([]Breach, error) {
	a.calls++
	ids := make([]string, 0, len(terms))
	for _, t := range terms {
		ids = append(ids, t.ID)
	}
	a.asked = append(a.asked, ids)
	if a.err != nil {
		return nil, a.err
	}
	var out []Breach
	for _, t := range terms {
		if detail, ok := a.breach[t.ID]; ok {
			out = append(out, Breach{ID: t.ID, Detail: detail})
		}
	}
	return out, nil
}

var _ InvariantAuditor = (*fakeAuditor)(nil)

// --- harness ----------------------------------------------------------------

type termHarness struct {
	*harness
	au *fakeAuditor
}

func newTermHarness(t *testing.T, stop StopEvaluator, opts ...Option) *termHarness {
	t.Helper()
	au := newFakeAuditor()
	return &termHarness{harness: newHarness(t, stop, append(opts, WithInvariantAudit(au))...), au: au}
}

// shippedStop reports the stop condition met once the goal has taken a step, and counts
// how often it was asked. Counting is how the ordering is proven: an evaluator that is
// never consulted over a broken term cannot have been overridden by one.
type shippedStop struct{ calls int }

func (s *shippedStop) Met(_ context.Context, _ Spec, st Status) (bool, string, error) {
	s.calls++
	return st.Steps >= 1, "the change is shipped", nil
}

var _ StopEvaluator = (*shippedStop)(nil)

// termSpec is the goal these tests run: the stated terms, and a stop condition an
// evaluator is free to say is met.
func termSpec(invs ...Invariant) Spec {
	return Spec{Objective: "ship the change", StopCondition: "the change is shipped", Invariants: invs}
}

func noForcePush() Invariant {
	return Invariant{ID: "no-force-push", Statement: "never force-push a shared branch"}
}

// start reconciles a fresh goal to its first dispatched step.
func (h *termHarness) start(t *testing.T, ref reconcile.Ref) {
	t.Helper()
	h.reconcile(t, ref) // finalizer, then dispatch
	if st := h.status(t, ref); st.InFlight == nil {
		t.Fatalf("no step was dispatched: %+v", st)
	}
}

// step completes the dispatched step and reconciles again, which is the pass that
// observes the completion and audits the terms against it.
func (h *termHarness) step(t *testing.T, ref reconcile.Ref) {
	t.Helper()
	h.start(t, ref)
	h.completeStep(t)
	h.reconcile(t, ref)
}

// --- the ordering -------------------------------------------------------------

// TestBrokenTermStopsTheGoalBeforeTheStopEvaluatorIsAsked is the property the whole
// design rests on. The evaluator is standing by to say the objective was achieved, and
// it is never consulted, because a term of the run was broken getting there. An
// evaluator that is never asked cannot have been overridden, which makes this an
// ordering rather than a check someone remembered to write.
func TestBrokenTermStopsTheGoalBeforeTheStopEvaluatorIsAsked(t *testing.T) {
	stop := &shippedStop{}
	h := newTermHarness(t, stop)
	h.au.breach["no-force-push"] = "force-pushed origin/main at step 1"
	ref := h.createGoal(t, "g", termSpec(noForcePush()))

	h.start(t, ref)
	// The evaluator has been asked once, over a goal that had done nothing, and said
	// no. From here it would say the objective was achieved.
	asked := stop.calls
	h.completeStep(t)
	h.reconcile(t, ref)

	st := h.status(t, ref)
	if st.Phase != PhaseStalled {
		t.Fatalf("phase = %q, want Stalled: %+v", st.Phase, st)
	}
	if stop.calls != asked {
		t.Fatalf("the stop evaluator was asked over a broken term (%d then %d)", asked, stop.calls)
	}
	if !strings.Contains(st.Message, "never force-push a shared branch") ||
		!strings.Contains(st.Message, "force-pushed origin/main at step 1") {
		t.Fatalf("the stall does not say which term was broken or how: %q", st.Message)
	}
	if !hasCond(st, CondStalled, "True") {
		t.Fatalf("no Stalled condition: %+v", st.Conditions)
	}
	for _, c := range st.Conditions {
		if c.Type == CondStalled && c.Reason != "InvariantBreached" {
			t.Fatalf("stall reason = %q, want InvariantBreached", c.Reason)
		}
	}
	if st.InFlight != nil {
		t.Fatalf("a step was dispatched after the term was broken: %+v", st.InFlight)
	}
	entry, breached := st.BreachedInvariant()
	if !breached || entry.Detail != "force-pushed origin/main at step 1" {
		t.Fatalf("the breach was not recorded on the status: %+v", st.Invariants)
	}
}

// TestABreachOutlivesTheAuditThatFoundIt: the breach is on the record, so a later pass
// settles the goal from the record alone. Editing the spec afterwards (here, adding a
// term, which admission does allow) does not launder it out, and the evaluator is still
// never asked.
func TestABreachOutlivesTheAuditThatFoundIt(t *testing.T) {
	stop := &shippedStop{}
	h := newTermHarness(t, stop)
	h.au.breach["no-force-push"] = "force-pushed origin/main"
	ref := h.createGoal(t, "g", termSpec(noForcePush()))
	h.step(t, ref)

	before, asked := h.au.calls, stop.calls
	h.putSpec(t, ref, termSpec(noForcePush(), Invariant{ID: "extra", Statement: "and no rewriting history"}))
	h.reconcile(t, ref)

	st := h.status(t, ref)
	if st.Phase != PhaseStalled {
		t.Fatalf("an edited spec resumed a breached goal: %+v", st)
	}
	if stop.calls != asked {
		t.Fatalf("the stop evaluator was asked after the breach (%d then %d)", asked, stop.calls)
	}
	if h.au.calls != before {
		t.Fatalf("the auditor was re-asked about a term already found broken (%d then %d)", before, h.au.calls)
	}
}

// TestATermIsAuditedOncePerStep: the audit is paced by the work, not by the reconcile
// loop. A poll tick over an in-flight step and a resync of a settled goal add nothing to
// the record, so neither spends an auditor call.
func TestATermIsAuditedOncePerStep(t *testing.T) {
	h := newTermHarness(t, stopAfter{at: 3})
	ref := h.createGoal(t, "g", termSpec(noForcePush()))

	h.reconcile(t, ref) // finalizer and the first dispatch
	if h.au.calls != 0 {
		t.Fatalf("the auditor was asked before any step had run (%d)", h.au.calls)
	}
	h.reconcile(t, ref) // poll: the step is still in flight
	if h.au.calls != 0 {
		t.Fatalf("a poll over an in-flight step spent an audit (%d)", h.au.calls)
	}
	for i := 1; i <= 3; i++ {
		h.completeStep(t)
		h.reconcile(t, ref)
		if h.au.calls != i {
			t.Fatalf("after %d completed steps the auditor had been asked %d times", i, h.au.calls)
		}
	}
	if st := h.status(t, ref); st.Phase != PhaseConverged {
		t.Fatalf("a goal whose terms held did not converge: %+v", st)
	}
	if st := h.status(t, ref); st.Invariants[0].Audits != 3 || st.Invariants[0].LastAudited == nil {
		t.Fatalf("the record does not show the term was checked: %+v", st.Invariants[0])
	}
	// A resync of a settled goal re-reads the record and must not re-audit it.
	h.reconcile(t, ref)
	if h.au.calls != 3 {
		t.Fatalf("a resync of a converged goal spent an audit (%d)", h.au.calls)
	}
}

// TestAnAuditThatCouldNotRunIsNotAPass: an auditor that fails stops the pass where it
// failed. A transient failure is handed back for the retry ladder and leaves the goal
// running; anything else settles the goal. Neither reads as terms that hold, and in
// neither case is the stop evaluator reached, which is the point: an auditor that could
// not answer has said nothing about the terms, and a run whose guard is down is not a
// run that cleared its guard.
func TestAnAuditThatCouldNotRunIsNotAPass(t *testing.T) {
	t.Run("transient", func(t *testing.T) {
		stop := &shippedStop{}
		h := newTermHarness(t, stop)
		h.au.err = fault.New(fault.Transient, "auditor_unreachable", "the auditor could not be reached")
		ref := h.createGoal(t, "g", termSpec(noForcePush()))

		h.start(t, ref)
		asked := stop.calls
		h.completeStep(t)
		_, err := h.gr.Reconcile(h.ctx, ref)
		if err == nil {
			t.Fatal("a failed audit was treated as terms that hold")
		}
		if got := fault.Classify(err); got != fault.Transient {
			t.Fatalf("a failed audit classified %q, want %q so it retries", got, fault.Transient)
		}
		if stop.calls != asked {
			t.Fatalf("the stop evaluator was asked over an audit that did not run (%d then %d)", asked, stop.calls)
		}
		st := h.status(t, ref)
		if st.Phase == PhaseConverged || st.Phase == PhaseStalled {
			t.Fatalf("a transient audit failure settled the goal: %+v", st)
		}
		if _, breached := st.BreachedInvariant(); breached {
			t.Fatalf("a failed audit recorded a breach: %+v", st.Invariants)
		}

		// It recovers: the same goal converges once the auditor answers again.
		h.au.err = nil
		h.reconcile(t, ref)
		if st := h.status(t, ref); st.Phase != PhaseConverged {
			t.Fatalf("the goal did not carry on once the auditor came back: %+v", st)
		}
	})

	// An unclassified error is the shape a real caller's failure arrives in, and it
	// classifies Terminal, so the goal settles instead of retrying an auditor that is
	// not coming back. Fail-closed either way is the property; which way is the
	// auditor's to say.
	t.Run("unclassified", func(t *testing.T) {
		stop := &shippedStop{}
		h := newTermHarness(t, stop)
		h.au.err = errors.New("the auditor could not be reached")
		ref := h.createGoal(t, "g", termSpec(noForcePush()))

		h.start(t, ref)
		asked := stop.calls
		h.completeStep(t)
		h.reconcile(t, ref)

		st := h.status(t, ref)
		if st.Phase != PhaseStalled {
			t.Fatalf("an audit that failed for good left the goal running: %+v", st)
		}
		if !strings.Contains(st.Message, "the auditor could not be reached") {
			t.Fatalf("the stall does not say the audit failed: %q", st.Message)
		}
		if stop.calls != asked {
			t.Fatalf("the stop evaluator was asked over an audit that did not run (%d then %d)", asked, stop.calls)
		}
		if _, breached := st.BreachedInvariant(); breached {
			t.Fatalf("a failed audit recorded a breach: %+v", st.Invariants)
		}
	})
}

// --- the terms cannot be renegotiated ------------------------------------------

// TestATermCannotBeRelaxedUnderARunningGoal: dropping or rewording an adopted term is a
// terminal spec fault on the reconcile path, so it stops the run rather than quietly
// taking effect. Adding one is allowed, and the run carries on held to both.
func TestATermCannotBeRelaxedUnderARunningGoal(t *testing.T) {
	tests := []struct {
		name string
		spec Spec
	}{
		{name: "dropped", spec: termSpec()},
		{name: "reworded", spec: termSpec(Invariant{
			ID:        "no-force-push",
			Statement: "avoid force-pushing a shared branch where practical",
		})},
		{name: "check dropped", spec: termSpec(Invariant{
			ID: "no-force-push", Statement: "never force-push a shared branch",
		})},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newTermHarness(t, stopAfter{at: 99})
			adopted := noForcePush()
			adopted.Check = "git reflog for a non-fast-forward push"
			ref := h.createGoal(t, "g", termSpec(adopted))
			h.reconcile(t, ref)
			if st := h.status(t, ref); len(st.Invariants) != 1 {
				t.Fatalf("the term was not adopted: %+v", st.Invariants)
			}

			h.putSpec(t, ref, tc.spec)
			_, err := h.gr.reconcile(h.ctx, ref)
			if err == nil {
				t.Fatal("the run relaxed its own terms and carried on")
			}
			if !errors.Is(err, ErrInvariantRelaxed) {
				t.Fatalf("error %v does not carry ErrInvariantRelaxed", err)
			}
			if got := fault.Classify(err); got != fault.Terminal {
				t.Fatalf("relaxing a term classified %q, want %q", got, fault.Terminal)
			}
			if got := faultCode(t, err); got != "goal_invariant_relaxed" {
				t.Fatalf("fault code %q, want goal_invariant_relaxed", got)
			}
			// Through the exported entry point that fault settles the goal, so a run
			// whose terms were edited under it stops rather than carrying on under
			// terms nobody agreed to.
			h.reconcile(t, ref)
			if st := h.status(t, ref); st.Phase != PhaseStalled || !strings.Contains(st.Message, "relaxed") {
				t.Fatalf("the goal did not settle on the relaxed term: %+v", st)
			}
		})
	}
}

// A term may be added to a running goal, and from then on the run is held to both.
func TestATermCanBeAddedToARunningGoal(t *testing.T) {
	h := newTermHarness(t, stopAfter{at: 99})
	ref := h.createGoal(t, "g", termSpec(noForcePush()))
	h.step(t, ref)

	added := Invariant{ID: "no-history-rewrite", Statement: "never rewrite published history"}
	h.putSpec(t, ref, termSpec(noForcePush(), added))
	h.completeStep(t)
	h.reconcile(t, ref)

	st := h.status(t, ref)
	if len(st.Invariants) != 2 {
		t.Fatalf("the added term was not adopted: %+v", st.Invariants)
	}
	if got := h.au.asked[len(h.au.asked)-1]; len(got) != 2 {
		t.Fatalf("the last audit was asked about %v, want both terms", got)
	}
	if st.Phase == PhaseStalled {
		t.Fatalf("adding a term stalled the goal: %+v", st)
	}
}

// A spec whose terms could never be audited is refused before anything is dispatched.
func TestUnauditableTermsAreRefusedAtAdmission(t *testing.T) {
	h := newTermHarness(t, stopAfter{at: 99})
	ref := h.createGoal(t, "g", termSpec(
		Invariant{ID: "a", Statement: "one"},
		Invariant{ID: "a", Statement: "another"},
	))

	_, err := h.gr.reconcile(h.ctx, ref)
	if err == nil {
		t.Fatal("a goal with duplicate term ids was admitted")
	}
	if !errors.Is(err, ErrInvariantDuplicate) {
		t.Fatalf("error %v does not carry ErrInvariantDuplicate", err)
	}
	if got := faultCode(t, err); got != "goal_invariants_invalid" {
		t.Fatalf("fault code %q, want goal_invariants_invalid", got)
	}
	if st := h.status(t, ref); st.InFlight != nil {
		t.Fatalf("a step was dispatched under terms that could never be checked: %+v", st.InFlight)
	}
}

// --- the honest defaults --------------------------------------------------------

// TestAGoalWithNoTermsNeverAsksTheAuditor: a goal that states no terms is unchanged by
// all of this, including in what it spends.
func TestAGoalWithNoTermsNeverAsksTheAuditor(t *testing.T) {
	h := newTermHarness(t, stopAfter{at: 1})
	ref := h.createGoal(t, "g", Spec{Objective: "o", StopCondition: "c"})

	h.step(t, ref)

	if h.au.calls != 0 {
		t.Fatalf("the auditor was asked about a goal with no terms (%d)", h.au.calls)
	}
	if st := h.status(t, ref); st.Phase != PhaseConverged {
		t.Fatalf("a goal with no terms did not converge: %+v", st)
	}
}

// TestAGoalWithTermsAndNoAuditorStalls: the alternative would be a run that states terms,
// is never checked against them, and finishes looking exactly like a run whose terms
// held. The stall happens before the first step is dispatched, so nothing is spent under
// an assurance that was not there.
func TestAGoalWithTermsAndNoAuditorStalls(t *testing.T) {
	h := newHarness(t, stopAfter{at: 1})
	ref := h.createGoal(t, "g", termSpec(noForcePush()))

	h.reconcile(t, ref)

	st := h.status(t, ref)
	if st.Phase != PhaseStalled {
		t.Fatalf("a goal with terms nobody checks kept running: %+v", st)
	}
	if !strings.Contains(st.Message, "no auditor") {
		t.Fatalf("the stall does not say why: %q", st.Message)
	}
	if st.InFlight != nil {
		t.Fatalf("a step was dispatched before the goal stalled: %+v", st.InFlight)
	}
	// The terms are still adopted and still cannot be relaxed, so the record of what
	// this run was to be held to survives the missing auditor.
	if len(st.Invariants) != 1 || st.Invariants[0].Audits != 0 {
		t.Fatalf("the term was not adopted, or was audited by nobody: %+v", st.Invariants)
	}
	h.putSpec(t, ref, termSpec())
	if _, err := h.gr.reconcile(h.ctx, ref); !errors.Is(err, ErrInvariantRelaxed) {
		t.Fatalf("with no auditor wired, dropping a term = %v, want ErrInvariantRelaxed", err)
	}
}

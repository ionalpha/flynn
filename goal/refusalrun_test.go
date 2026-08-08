package goal

import (
	"context"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/reconcile"
	"github.com/ionalpha/flynn/resource"
)

// fakeRefusals hands back a fixed record and counts how often it was read, so a test can
// prove the read is paced by completed steps rather than by reconcile ticks.
type fakeRefusals struct {
	record []Refusal
	err    error
	calls  int
}

func (f *fakeRefusals) Refusals(context.Context, resource.Resource) ([]Refusal, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.record, nil
}

var _ RefusalProbe = (*fakeRefusals)(nil)

// routedAround is the record of a run that met one gate and tried three doors.
func routedAround() []Refusal {
	return refusals(
		"capability_denied", "write_file",
		"capability_denied", "bash",
		"capability_denied", "mcp.fs.write",
	)
}

type refusalHarness struct {
	*harness
	probe *fakeRefusals
}

func newRefusalHarness(t *testing.T, stop StopEvaluator, record []Refusal) *refusalHarness {
	t.Helper()
	p := &fakeRefusals{record: record}
	return &refusalHarness{harness: newHarness(t, stop, WithRefusalProbe(p)), probe: p}
}

// step runs the goal to its first dispatched step, completes it, and reconciles again,
// which is the pass that reads the refusals against the step that just finished.
func (h *refusalHarness) step(t *testing.T, ref reconcile.Ref) {
	t.Helper()
	h.reconcile(t, ref)
	if st := h.status(t, ref); st.InFlight == nil {
		t.Fatalf("no step was dispatched: %+v", st)
	}
	h.completeStep(t)
	h.reconcile(t, ref)
}

// evaluatorSilent asserts the stop evaluator was not consulted on the pass that read the
// refusals. It is counted from the dispatch pass rather than from zero, because a goal is
// asked whether it is already done before it dispatches anything, and that call happens
// before there is a completed step for the refusals to be read against.
func evaluatorSilent(t *testing.T, stop *shippedStop, before int, why string) {
	t.Helper()
	if stop.calls != before {
		t.Errorf("the stop evaluator was asked %d more time(s) %s", stop.calls-before, why)
	}
}

func refusalSpec() Spec {
	return Spec{Objective: "ship the change", StopCondition: "the change is shipped"}
}

// TestRoutedAroundGateStopsBeforeTheStopEvaluatorIsAsked is the property the design rests
// on, and it is why this sits with the invariant audit rather than among the stall guards
// at the bottom of the pass. Those run only once the evaluator has said the goal is not
// finished, so a run that got where it was going would sail past every one of them. Here
// the evaluator is standing by to say the objective was achieved the moment a step lands,
// and it is not consulted at all, because the route was refused three times. An evaluator
// that is never asked cannot have been overridden.
func TestRoutedAroundGateStopsBeforeTheStopEvaluatorIsAsked(t *testing.T) {
	stop := &shippedStop{}
	h := newRefusalHarness(t, stop, routedAround())
	ref := h.createGoal(t, "g", refusalSpec())

	h.reconcile(t, ref)
	before := stop.calls
	h.completeStep(t)
	h.reconcile(t, ref)

	st := h.status(t, ref)
	if st.Phase != PhaseStalled {
		t.Fatalf("phase = %q, want %q: a run that routed around a gate converged", st.Phase, PhaseStalled)
	}
	evaluatorSilent(t, stop, before, "about a run that reached its objective through a gate that refused it")
	for _, want := range []string{"capability_denied", "write_file", "bash", "mcp.fs.write"} {
		if !strings.Contains(st.Message, want) {
			t.Errorf("message %q does not name %q", st.Message, want)
		}
	}
	if !hasCond(st, CondStalled, "True") {
		t.Errorf("conditions = %+v, want a stalled condition", st.Conditions)
	}
	for _, c := range st.Conditions {
		if c.Type == CondStalled && c.Reason != "RefusedRouteAround" {
			t.Errorf("stalled reason = %q, want RefusedRouteAround so the stop is not read as "+
				"a budget running out", c.Reason)
		}
	}
}

// TestRefusalsBelowTheLimitLeaveTheRunAlone is the case every ordinary run is in: a gate
// said no once, the run did something else, and nothing about that is a verdict.
func TestRefusalsBelowTheLimitLeaveTheRunAlone(t *testing.T) {
	stop := &shippedStop{}
	h := newRefusalHarness(t, stop, refusals("capability_denied", "write_file"))
	ref := h.createGoal(t, "g", refusalSpec())

	h.step(t, ref)

	if st := h.status(t, ref); st.Phase != PhaseConverged {
		t.Fatalf("phase = %q, want %q: a single refusal stopped a run that finished", st.Phase, PhaseConverged)
	}
	if stop.calls == 0 {
		t.Error("the stop evaluator was never asked about a run with one refusal against it")
	}
}

// TestRefusalsAreReadOncePerCompletedStep pins the pacing. A poll tick and a resync see the
// record the last pass already ruled on, so re-reading them would buy nothing; only a
// completed step can have added a refusal.
func TestRefusalsAreReadOncePerCompletedStep(t *testing.T) {
	h := newRefusalHarness(t, &neverStop{}, nil)
	ref := h.createGoal(t, "g", refusalSpec())

	h.reconcile(t, ref) // finalizer and first dispatch: nothing has completed
	h.reconcile(t, ref) // the step is still in flight: a poll, not a step
	if h.probe.calls != 0 {
		t.Fatalf("the record was read %d times before any step completed", h.probe.calls)
	}
	h.completeStep(t)
	h.reconcile(t, ref)
	if h.probe.calls != 1 {
		t.Fatalf("the record was read %d times for one completed step, want 1", h.probe.calls)
	}
}

// TestUnreadableRecordDoesNotPass is the direction this must never fail in. A record nobody
// could read says nothing about whether the run was refused, and treating it as a clean run
// would mean the one thing that turns the guard off is the record being unavailable.
func TestUnreadableRecordDoesNotPass(t *testing.T) {
	stop := &shippedStop{}
	h := newRefusalHarness(t, stop, nil)
	h.probe.err = fault.New(fault.Transient, "refusal_history_read", "the log is unreachable")
	ref := h.createGoal(t, "g", refusalSpec())

	h.reconcile(t, ref)
	before := stop.calls
	h.completeStep(t)
	_, err := h.gr.Reconcile(h.ctx, ref)
	if err == nil {
		t.Fatal("a run whose refusals could not be read carried on")
	}
	if got := fault.Classify(err); got != fault.Transient {
		t.Errorf("class = %q, want the probe's own %q: a momentary read failure must retry, "+
			"not stall a healthy goal", got, fault.Transient)
	}
	evaluatorSilent(t, stop, before, "over a record nobody could read")
	if st := h.status(t, ref); st.Phase == PhaseConverged {
		t.Error("a run whose refusals could not be read converged")
	}
}

// TestNoProbeLeavesDetectionOff is the host that has not wired one. Unlike a goal's stated
// terms, which stall a goal when nothing is there to check them, nothing on a spec asks for
// this, so there is no promise being broken.
func TestNoProbeLeavesDetectionOff(t *testing.T) {
	stop := &shippedStop{}
	h := newHarness(t, stop)
	ref := h.createGoal(t, "g", refusalSpec())

	h.reconcile(t, ref)
	h.completeStep(t)
	h.reconcile(t, ref)

	if st := h.status(t, ref); st.Phase != PhaseConverged {
		t.Fatalf("phase = %q, want %q with no probe wired", st.Phase, PhaseConverged)
	}
}

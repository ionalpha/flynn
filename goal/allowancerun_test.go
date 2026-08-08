package goal

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/allowance"
	"github.com/ionalpha/flynn/resource"
)

// stallCondition renders the Stalled condition as reason/status, so a test can tell a pause
// (True) from the pause having been answered (False), which stalledReason cannot: it reports
// only conditions that are True.
func stallCondition(st Status) string {
	for _, c := range st.Conditions {
		if c.Type == CondStalled {
			return c.Reason + "/" + c.Status
		}
	}
	return ""
}

// TestAnUndeclaredIrreversibleActionPausesBeforeTheStopEvaluatorIsAsked is the ordering the
// pause depends on. A run refused an irreversible action and then reporting the objective
// achieved has either found another way to it or is calling it met without the part it was
// refused, and asking the evaluator first would be taking its word for which. Here the
// evaluator is standing by to say the goal is done and it is not consulted at all.
func TestAnUndeclaredIrreversibleActionPausesBeforeTheStopEvaluatorIsAsked(t *testing.T) {
	stop := &shippedStop{}
	h := newRefusalHarness(t, stop, allowanceRefused("fs.delete"))
	ref := h.createGoal(t, "g", refusalSpec())

	h.reconcile(t, ref)
	before := stop.calls
	h.completeStep(t)
	h.reconcile(t, ref)

	st := h.status(t, ref)
	if st.Phase != PhaseStalled {
		t.Fatalf("phase = %q, want %q: a run that was refused an undeclared irreversible "+
			"action converged", st.Phase, PhaseStalled)
	}
	evaluatorSilent(t, stop, before, "about a run stopped on an authority nobody gave it")
	if got := stallCondition(st); got != AllowanceStallReason+"/True" {
		t.Errorf("stalled condition = %q, want %s/True: a pause that reads as an ordinary "+
			"stall is a question nobody knows to answer", got, AllowanceStallReason)
	}
	if !strings.Contains(st.Message, "fs.delete") {
		t.Errorf("message %q does not name the action to declare", st.Message)
	}
}

// TestDeclaringTheAllowanceReleasesThePause is the answer arriving. The refusal stays on the
// record, because it happened; the spec now says the run may do it, so the same record no
// longer reads as an open question and the goal picks up where it stopped.
func TestDeclaringTheAllowanceReleasesThePause(t *testing.T) {
	stop := &shippedStop{}
	h := newRefusalHarness(t, stop, allowanceRefused("fs.delete"))
	ref := h.createGoal(t, "g", refusalSpec())

	h.reconcile(t, ref)
	h.completeStep(t)
	h.reconcile(t, ref)
	if st := h.status(t, ref); st.Phase != PhaseStalled {
		t.Fatalf("phase = %q, want the goal paused first", st.Phase)
	}

	spec := refusalSpec()
	spec.Allowances = []Allowance{{Action: "fs.delete"}}
	h.putSpec(t, ref, spec)
	h.reconcile(t, ref)

	st := h.status(t, ref)
	if st.Phase == PhaseStalled {
		t.Fatalf("the goal stayed paused after its author declared the allowance: %q", st.Message)
	}
	if got := stallCondition(st); got != AllowanceResumeReason+"/False" {
		t.Errorf("stalled condition = %q, want %s/False: a working goal that still describes "+
			"itself as blocked on an answered question is worse than no condition at all",
			got, AllowanceResumeReason)
	}
}

// TestAPausedGoalRestsUntilItsSpecChanges is why the pause is a place to stop rather than a
// loop. Nothing about a paused goal changes on a poll, so it is not re-read until the thing
// that could answer it, its spec, is edited.
func TestAPausedGoalRestsUntilItsSpecChanges(t *testing.T) {
	h := newRefusalHarness(t, &shippedStop{}, allowanceRefused("fs.delete"))
	ref := h.createGoal(t, "g", refusalSpec())

	h.reconcile(t, ref)
	h.completeStep(t)
	h.reconcile(t, ref)
	reads := h.probe.calls

	h.reconcile(t, ref)
	if h.probe.calls != reads {
		t.Errorf("the record was re-read %d time(s) for a paused goal nothing had answered",
			h.probe.calls-reads)
	}
}

// TestARoutedAroundRunIsNotHandedAnAsk is the ranking where both readings fire. A run
// refused three different irreversible actions has met the substitution shape, and telling
// its author to widen the authority of a run that was looking for a way through is the
// opposite of what that run needs.
func TestARoutedAroundRunIsNotHandedAnAsk(t *testing.T) {
	record := allowanceRefused("fs.delete", "shell", "net.post")
	h := newRefusalHarness(t, &shippedStop{}, record)
	ref := h.createGoal(t, "g", refusalSpec())

	h.reconcile(t, ref)
	h.completeStep(t)
	h.reconcile(t, ref)

	st := h.status(t, ref)
	if st.Phase != PhaseStalled {
		t.Fatalf("phase = %q, want the run stopped", st.Phase)
	}
	if got := stallCondition(st); got != "RefusedRouteAround/True" {
		t.Errorf("stalled condition = %q, want RefusedRouteAround/True: the verdict is the "+
			"truer account of a run that tried three doors", got)
	}
}

// TestAGoalThatDeclaresNothingIsUnaffected is every run that predates this. No refusal for
// want of a declaration means no ask, whatever else the run was refused.
func TestAGoalThatDeclaresNothingIsUnaffected(t *testing.T) {
	h := newRefusalHarness(t, &shippedStop{}, refusals("capability_denied", "write_file"))
	ref := h.createGoal(t, "g", refusalSpec())

	h.step(t, ref)

	if st := h.status(t, ref); st.Phase != PhaseConverged {
		t.Fatalf("phase = %q, want %q: an ordinary refusal paused a run that finished",
			st.Phase, PhaseConverged)
	}
}

// TestADeclarationNamingNoActionIsRefusedBeforeItIsStored keeps the authorization that
// authorizes nothing out of the record entirely, rather than leaving it to be discovered by
// a run already part-way through work under it.
func TestADeclarationNamingNoActionIsRefusedBeforeItIsStored(t *testing.T) {
	h := newHarness(t, &shippedStop{})
	spec := refusalSpec()
	spec.Allowances = []Allowance{{Target: "prod"}}
	raw, _ := json.Marshal(spec)

	_, err := h.store.Put(h.ctx, resource.Resource{
		APIVersion: GroupVersion, Kind: Kind, Name: "g", Spec: raw,
	})
	if err == nil {
		t.Fatal("a goal carrying an allowance that names no action was stored")
	}
}

// TestPauseReadsTheSameRecordTheVerdictDoes pins the shared probe. One read per completed
// step answers both questions, so a host that wired the refusal probe has the pause too and
// there is no second port to forget.
func TestPauseReadsTheSameRecordTheVerdictDoes(t *testing.T) {
	h := newRefusalHarness(t, &shippedStop{}, allowanceRefused("fs.delete"))
	ref := h.createGoal(t, "g", refusalSpec())

	h.reconcile(t, ref)
	h.completeStep(t)
	h.reconcile(t, ref)

	if h.probe.calls != 1 {
		t.Fatalf("the record was read %d times for one completed step, want 1", h.probe.calls)
	}
}

// TestNoProbeLeavesThePauseOff matches the verdict's rule for a host that wired nothing. The
// gate at the waist still refuses the undeclared action, so nothing runs undeclared; what a
// host without a probe loses is the run stopping to ask rather than carrying on around it.
func TestNoProbeLeavesThePauseOff(t *testing.T) {
	h := newHarness(t, &shippedStop{})
	ref := h.createGoal(t, "g", refusalSpec())

	h.reconcile(t, ref)
	h.completeStep(t)
	h.reconcile(t, ref)

	if st := h.status(t, ref); st.Phase != PhaseConverged {
		t.Fatalf("phase = %q, want %q with no probe wired", st.Phase, PhaseConverged)
	}
}

// TestTheAskIsWrittenForThePersonAnsweringIt guards the wording at the point it is produced,
// because the reason string is the entire interface of a paused run.
func TestTheAskIsWrittenForThePersonAnsweringIt(t *testing.T) {
	h := newRefusalHarness(t, &shippedStop{},
		[]Refusal{{Rule: allowance.CodeRequired, Action: "fs.delete"}})
	ref := h.createGoal(t, "g", refusalSpec())

	h.reconcile(t, ref)
	h.completeStep(t)
	h.reconcile(t, ref)

	msg := h.status(t, ref).Message
	for _, want := range []string{"fs.delete", "cannot be undone", "declare an allowance"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not say %q", msg, want)
		}
	}
}

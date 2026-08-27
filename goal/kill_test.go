package goal

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/jobs"
)

// --- fakes ------------------------------------------------------------------

// fakeHalter records what it was asked to halt and lets a test observe the record at the
// moment it is engaged, which is how the ordering rule below is proved rather than
// asserted.
type fakeHalter struct {
	runs    []string
	reasons []string
	onFirst func()
}

func (h *fakeHalter) Engage(run, reason string) {
	h.runs = append(h.runs, run)
	h.reasons = append(h.reasons, reason)
	if len(h.runs) == 1 && h.onFirst != nil {
		h.onFirst()
	}
}

func (h *fakeHalter) ProveHalts() error { return nil }

var _ Halter = (*fakeHalter)(nil)

// neverDoneStop keeps the goal running and counts how often it was asked, so a test can
// prove a killed run never reached it.
type neverDoneStop struct{ calls int }

func (s *neverDoneStop) Met(context.Context, Spec, Status) (bool, string, error) {
	s.calls++
	return false, "", nil
}

// alwaysDoneStop reports the objective achieved every time it is asked. A killed run must
// not be handed to it: a stop the operator ordered outranks the verdict that the run had
// finished anyway.
type alwaysDoneStop struct{ calls int }

func (s *alwaysDoneStop) Met(context.Context, Spec, Status) (bool, string, error) {
	s.calls++
	return true, "the trail is written", nil
}

func killSpec(k *Kill) Spec {
	return Spec{Objective: "add the audit trail", StopCondition: "the trail is written", Kill: k}
}

// --- the stop ---------------------------------------------------------------

// TestAKilledRunSettlesAsKilledWithTheOperatorsReason is what the whole feature comes to: a
// run somebody stopped settles under its own reason, carrying what they said, and the halt
// is engaged against the run's own id.
func TestAKilledRunSettlesAsKilledWithTheOperatorsReason(t *testing.T) {
	halt := &fakeHalter{}
	h := newHarness(t, &neverDoneStop{}, WithHalt(halt))
	ref := h.createGoal(t, "g", killSpec(nil))
	h.reconcile(t, ref) // finalizer, then dispatch a step

	h.putSpec(t, ref, killSpec(&Kill{Reason: "it is editing the wrong repository"}))
	h.reconcile(t, ref)

	st := h.status(t, ref)
	if st.Phase != PhaseStalled {
		t.Fatalf("phase = %q, want %q", st.Phase, PhaseStalled)
	}
	if st.Killed == nil {
		t.Fatal("status does not record that the run was killed")
	}
	if !strings.Contains(st.Message, "it is editing the wrong repository") {
		t.Fatalf("message = %q, want the operator's reason in it", st.Message)
	}
	cond, ok := findCondition(st.Conditions, CondStalled)
	if !ok || cond.Reason != KilledReason {
		t.Fatalf("stalled condition = %+v, want reason %q", cond, KilledReason)
	}
	if len(halt.runs) != 1 || halt.runs[0] != "g" {
		t.Fatalf("halted %v, want the run's own id once", halt.runs)
	}
	if !strings.Contains(halt.reasons[0], "it is editing the wrong repository") {
		t.Fatalf("halt reason = %q, want the operator's reason in it", halt.reasons[0])
	}
}

// TestAKillWithNoReasonStillStopsTheRun: the reason is the courtesy and the order is the
// content, so a kill with nothing written in it is complete.
func TestAKillWithNoReasonStillStopsTheRun(t *testing.T) {
	h := newHarness(t, &neverDoneStop{}, WithHalt(&fakeHalter{}))
	ref := h.createGoal(t, "g", killSpec(&Kill{}))
	h.reconcile(t, ref)

	st := h.status(t, ref)
	if st.Phase != PhaseStalled || st.Killed == nil {
		t.Fatalf("phase = %q, killed = %v; want a stopped, killed run", st.Phase, st.Killed)
	}
	if st.Message != "stopped by the operator" {
		t.Fatalf("message = %q, want the bare account", st.Message)
	}
}

// TestTheHaltIsEngagedBeforeTheRecordSaysTheRunStopped is the ordering the durability
// argument rests on. The prompt half runs first, so a crash between the two leaves a run
// halted against a record that has not moved, and the next pass reads the order again. The
// other order would leave a settled record over a run still dispatching actions.
func TestTheHaltIsEngagedBeforeTheRecordSaysTheRunStopped(t *testing.T) {
	var phaseAtEngage Phase
	halt := &fakeHalter{}
	h := newHarness(t, &neverDoneStop{}, WithHalt(halt))
	ref := h.createGoal(t, "g", killSpec(nil))
	h.reconcile(t, ref)
	halt.onFirst = func() { phaseAtEngage = h.status(t, ref).Phase }

	h.putSpec(t, ref, killSpec(&Kill{Reason: "stop"}))
	h.reconcile(t, ref)

	if phaseAtEngage == PhaseStalled {
		t.Fatal("the record already said the run had stopped when the halt was engaged")
	}
	if h.status(t, ref).Phase != PhaseStalled {
		t.Fatal("the run was halted and the record never caught up")
	}
}

// TestAKilledRunDoesNotWaitForItsStepToFinish: the step in flight is abandoned, not
// awaited. Its actions are being refused at the waist from the moment the halt engaged, so
// a goal that went on recording it as in flight would read as still working.
func TestAKilledRunDoesNotWaitForItsStepToFinish(t *testing.T) {
	stop := &neverDoneStop{}
	h := newHarness(t, stop, WithHalt(&fakeHalter{}))
	ref := h.createGoal(t, "g", killSpec(nil))
	h.reconcile(t, ref) // dispatches a step and leaves it in flight
	if h.status(t, ref).InFlight == nil {
		t.Fatal("no step in flight to be killed mid-way")
	}
	before := stop.calls

	h.putSpec(t, ref, killSpec(&Kill{Reason: "stop"}))
	h.reconcile(t, ref)

	st := h.status(t, ref)
	if st.InFlight != nil {
		t.Fatalf("in-flight step still recorded after the kill: %+v", st.InFlight)
	}
	if st.Phase != PhaseStalled {
		t.Fatalf("phase = %q, want the run stopped without waiting for the step", st.Phase)
	}
	if stop.calls != before {
		t.Fatalf("the stop evaluator was asked %d more times; a killed run has no verdict to reach", stop.calls-before)
	}
}

// TestAKillOutranksACompletionVerdict: an evaluator that would say the objective was
// achieved is never reached. Whether the run would have finished anyway is not the
// question a kill asks, and a killed run that recorded itself converged would be a record
// of an outcome nobody chose.
func TestAKillOutranksACompletionVerdict(t *testing.T) {
	stop := &alwaysDoneStop{}
	h := newHarness(t, stop, WithHalt(&fakeHalter{}))
	ref := h.createGoal(t, "g", killSpec(&Kill{Reason: "stop"}))
	h.reconcile(t, ref)

	if st := h.status(t, ref); st.Phase != PhaseStalled {
		t.Fatalf("phase = %q, want %q", st.Phase, PhaseStalled)
	}
	if stop.calls != 0 {
		t.Fatalf("the stop evaluator was asked %d times about a killed run", stop.calls)
	}
}

// TestAKillStopsAParkedRun: a goal waiting on its children is stopped like any other. The
// park is a statement about the work, and a kill is not about the work.
func TestAKillStopsAParkedRun(t *testing.T) {
	h := newHarness(t, &neverDoneStop{}, WithHalt(&fakeHalter{}))
	ref := h.createGoal(t, "g", killSpec(nil))
	h.reconcile(t, ref)
	h.setStatus(t, ref, func(st *Status) {
		now := h.clk.Now()
		st.WaitingSince = &now
	})

	h.putSpec(t, ref, killSpec(&Kill{Reason: "stop"}))
	h.reconcile(t, ref)

	st := h.status(t, ref)
	if st.Phase != PhaseStalled {
		t.Fatalf("phase = %q, want a parked run stopped", st.Phase)
	}
	if st.WaitingSince != nil {
		t.Fatal("the killed run is still recorded as waiting on something")
	}
}

// TestAKillWithNoHalterStillStopsTheRun is the honest weaker behaviour. Nothing here is a
// judgement the goal cannot make for itself, so an unwired halt costs promptness and not
// the stop.
func TestAKillWithNoHalterStillStopsTheRun(t *testing.T) {
	h := newHarness(t, &neverDoneStop{})
	ref := h.createGoal(t, "g", killSpec(&Kill{Reason: "stop"}))
	h.reconcile(t, ref)

	if st := h.status(t, ref); st.Phase != PhaseStalled || st.Killed == nil {
		t.Fatalf("phase = %q, killed = %v; want the run stopped anyway", st.Phase, st.Killed)
	}
}

// TestAKilledRunIsNotReconciledAgain: the stop is settled, so the goal is left alone until
// its spec changes. Without this a killed run would be re-engaged and re-written on every
// resync tick for the life of the process.
func TestAKilledRunIsNotReconciledAgain(t *testing.T) {
	halt := &fakeHalter{}
	h := newHarness(t, &neverDoneStop{}, WithHalt(halt))
	ref := h.createGoal(t, "g", killSpec(&Kill{Reason: "stop"}))
	h.reconcile(t, ref)
	h.reconcile(t, ref)
	h.reconcile(t, ref)

	if len(halt.runs) != 1 {
		t.Fatalf("halted %d times, want once: a settled goal is not re-decided", len(halt.runs))
	}
}

// --- the no-withdrawal rule -------------------------------------------------

// TestAKillCannotBeWithdrawn is the rule that makes the record trustworthy, and it is what
// stops a killed run coming back to life. Taking the order off the spec changes the spec
// hash, so the goal is looked at again rather than skipped as settled; without the rule
// that pass would find nothing stopping it and dispatch the next step, and the run would
// resume with its record still saying a person had stopped it.
func TestAKillCannotBeWithdrawn(t *testing.T) {
	h := newHarness(t, &neverDoneStop{}, WithHalt(&fakeHalter{}))
	ref := h.createGoal(t, "g", killSpec(&Kill{Reason: "stop"}))
	h.reconcile(t, ref)
	drainJobs(t, h)

	h.putSpec(t, ref, killSpec(nil))
	h.reconcile(t, ref)

	st := h.status(t, ref)
	if st.Phase != PhaseStalled || st.Killed == nil {
		t.Fatalf("phase = %q, killed = %v; the withdrawal restarted a stopped run", st.Phase, st.Killed)
	}
	if !strings.Contains(st.Message, "stopped by the operator") {
		t.Fatalf("message = %q, want the operator's account kept", st.Message)
	}
	if st.InFlight != nil {
		t.Fatalf("a step was dispatched after the kill was withdrawn: %+v", st.InFlight)
	}
}

// TestAdmitRefusesAWithdrawnKill checks the rule itself, at the point it is enforced, as a
// terminal fault. The reconcile above proves the consequence; this proves the refusal is a
// spec fault and not an incidental no-op.
func TestAdmitRefusesAWithdrawnKill(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	status := Status{Killed: &now}
	err := admit(killSpec(nil), &status)
	if !errors.Is(err, ErrKillWithdrawn) {
		t.Fatalf("admit = %v, want %v", err, ErrKillWithdrawn)
	}
	if fault.Classify(err) != fault.Terminal {
		t.Fatalf("class = %s, want terminal: no retry fixes an edited spec", fault.Classify(err))
	}
}

// drainJobs claims and completes whatever the reconciler dispatched, so a later assertion
// about an in-flight step is about a step dispatched after the point under test.
func drainJobs(t *testing.T, h *harness) {
	t.Helper()
	claimed, err := h.jobs.Claim(h.ctx, jobs.ClaimParams{Queue: StepQueue, Limit: 10, LeaseFor: int64(time.Minute)})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	for _, j := range claimed {
		if err := h.jobs.Complete(h.ctx, j.ID); err != nil {
			t.Fatalf("complete: %v", err)
		}
	}
}

// TestValidateKillGivenIsQuietAboutEveryOtherCase: a run nobody killed, and a kill still on
// the spec, both pass. The rule catches one edit and does not become a second way for a
// well-formed spec to be refused.
func TestValidateKillGivenIsQuietAboutEveryOtherCase(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		status Status
		kill   *Kill
		want   error
	}{
		{"never killed, no order", Status{}, nil, nil},
		{"never killed, order just arrived", Status{}, &Kill{}, nil},
		{"killed, order still there", Status{Killed: &now}, &Kill{Reason: "stop"}, nil},
		{"killed, order removed", Status{Killed: &now}, nil, ErrKillWithdrawn},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := c.status.ValidateKillGiven(c.kill); !errors.Is(err, c.want) {
				t.Fatalf("ValidateKillGiven = %v, want %v", err, c.want)
			}
		})
	}
}

// TestKillMessageSaysWhoStoppedTheRun: the message is read by a person handed a stopped
// run, so it says a person stopped it whether or not they said why.
func TestKillMessageSaysWhoStoppedTheRun(t *testing.T) {
	if got := KillMessage(Kill{}); got != "stopped by the operator" {
		t.Fatalf("KillMessage(bare) = %q", got)
	}
	if got := KillMessage(Kill{Reason: "  wrong branch  "}); got != "stopped by the operator: wrong branch" {
		t.Fatalf("KillMessage(reason) = %q", got)
	}
}

// findCondition returns the condition of the given type.
func findCondition(conds []Condition, typ string) (Condition, bool) {
	for _, c := range conds {
		if c.Type == typ {
			return c, true
		}
	}
	return Condition{}, false
}

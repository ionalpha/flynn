package goal

import (
	"context"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/reconcile"
	"github.com/ionalpha/flynn/resource"
)

// --- pure streak logic ------------------------------------------------------

// TestObserveProgressResetsOnAnyChangedFingerprint: a fingerprint different from the
// last mark is progress, and the streak resets to zero however high it had climbed. This
// is the "reset on any signal" half of the rule.
func TestObserveProgressResetsOnAnyChangedFingerprint(t *testing.T) {
	var s Status
	s.ObserveProgress("a", "did a") // baseline
	if got := s.ObserveProgress("a", "did a again"); got != 1 {
		t.Fatalf("identical fingerprint: streak = %d, want 1", got)
	}
	if got := s.ObserveProgress("a", "still a"); got != 2 {
		t.Fatalf("identical fingerprint: streak = %d, want 2", got)
	}
	if got := s.ObserveProgress("b", "did b"); got != 0 {
		t.Fatalf("changed fingerprint did not reset the streak: streak = %d, want 0", got)
	}
	if s.LastActivity != "did b" {
		t.Fatalf("LastActivity = %q, want %q", s.LastActivity, "did b")
	}
}

// TestObserveProgressFirstStepIsNeverIdle: the baseline-establishing step is progress,
// never an idle step, so a goal is not charged for the step that set the mark.
func TestObserveProgressFirstStepIsNeverIdle(t *testing.T) {
	var s Status
	if got := s.ObserveProgress("anything", "first"); got != 0 {
		t.Fatalf("first step streak = %d, want 0", got)
	}
	if s.StalledForNoProgress() {
		t.Fatal("a goal stalled on its first observed step")
	}
}

// TestNSimilarButRealStepsDoNotTrip is the trap the task calls out explicitly: N
// legitimately similar steps that are each real work must NOT trip detection. Each step
// here does something genuinely different (a distinct fingerprint), the way real work
// reads different files or writes different content, so the streak never leaves zero even
// well past the limit.
func TestNSimilarButRealStepsDoNotTrip(t *testing.T) {
	var s Status
	fingerprints := []string{"acts=1", "acts=2", "acts=3", "acts=4", "acts=5", "acts=6"}
	for i, fp := range fingerprints {
		s.ObserveProgress(fp, "real work")
		if s.StalledForNoProgress() {
			t.Fatalf("real work tripped no-progress detection at step %d (streak %d)", i, s.IdleStreak)
		}
	}
}

// TestStalledForNoProgressAtTheLimit: the streak stalls exactly at NoProgressLimit
// consecutive idle steps, not before.
func TestStalledForNoProgressAtTheLimit(t *testing.T) {
	var s Status
	s.ObserveProgress("x", "baseline")
	for i := 1; i < NoProgressLimit; i++ {
		s.ObserveProgress("x", "idle")
		if s.StalledForNoProgress() {
			t.Fatalf("stalled early at streak %d (limit %d)", s.IdleStreak, NoProgressLimit)
		}
	}
	s.ObserveProgress("x", "idle")
	if !s.StalledForNoProgress() {
		t.Fatalf("did not stall at streak %d (limit %d)", s.IdleStreak, NoProgressLimit)
	}
}

// TestProgressWarningWindow: no warning below the warn point, a warning naming the streak
// from the warn point up to the limit, and no warning at the limit (the goal is stopped,
// not warned).
func TestProgressWarningWindow(t *testing.T) {
	if w := ProgressWarning(progressWarnAt - 1); w != "" {
		t.Fatalf("warning below the warn point: %q", w)
	}
	w := ProgressWarning(progressWarnAt)
	if w == "" || !strings.Contains(w, "will stop") {
		t.Fatalf("no usable warning at the warn point: %q", w)
	}
	if w := ProgressWarning(NoProgressLimit); w != "" {
		t.Fatalf("warned at the stopping limit instead of stopping: %q", w)
	}
}

// TestNoProgressReasonNamesTheLastActivity: the stall reason says what the goal was stuck
// doing, which is the difference between "no progress, stuck re-reading X" and a bare
// budget message.
func TestNoProgressReasonNamesTheLastActivity(t *testing.T) {
	var s Status
	s.ObserveProgress("x", "baseline")
	s.ObserveProgress("x", "re-reading config.yaml")
	reason := s.NoProgressReason()
	if !strings.Contains(reason, "re-reading config.yaml") {
		t.Fatalf("reason does not name the last activity: %q", reason)
	}
}

// TestNoProgressReasonWithoutALastActivity: when no activity was recorded, the reason
// still states the streak rather than dangling an empty "last doing" clause.
func TestNoProgressReasonWithoutALastActivity(t *testing.T) {
	s := Status{IdleStreak: NoProgressLimit} // no LastActivity set
	reason := s.NoProgressReason()
	if !strings.Contains(reason, "no progress") {
		t.Fatalf("reason is not a no-progress message: %q", reason)
	}
	if strings.Contains(reason, "last doing") {
		t.Fatalf("reason dangled an empty last-doing clause: %q", reason)
	}
}

// --- reconciler integration -------------------------------------------------

// fakeProbe returns a scripted fingerprint per call and a fixed summary, so a reconciler
// test can drive the streak deterministically without a spine.
type fakeProbe struct {
	fingerprints []string
	summary      string
	calls        int
	err          error
}

func (p *fakeProbe) Progress(context.Context, resource.Resource) (string, string, error) {
	if p.err != nil {
		return "", "", p.err
	}
	fp := p.fingerprints[len(p.fingerprints)-1]
	if p.calls < len(p.fingerprints) {
		fp = p.fingerprints[p.calls]
	}
	p.calls++
	return fp, p.summary, nil
}

// runSteps dispatches and completes n build steps, reconciling after each completion so
// the reconciler observes it.
func (h *harness) runSteps(t *testing.T, ref reconcile.Ref, n int) {
	t.Helper()
	for range n {
		h.reconcile(t, ref) // dispatch
		h.completeStep(t)   // worker finishes it
		h.reconcile(t, ref) // observe
	}
}

// TestReconcilerStallsOnNoProgress: a goal whose probe reports the same fingerprint every
// step stops with a no_progress reason once the streak reaches the limit, and the reason
// names what it was last doing — not a budget message, which is what a no-progress loop
// would otherwise reach.
func TestReconcilerStallsOnNoProgress(t *testing.T) {
	probe := &fakeProbe{fingerprints: []string{"idle"}, summary: "re-reading the same files"}
	h := newHarness(t, neverStop{}, WithProgressProbe(probe))
	ref := h.createGoal(t, "stuck", Spec{Objective: "loop forever", StopCondition: "never", MaxSteps: 50})

	// Baseline step, then NoProgressLimit idle steps: the last one trips the guard.
	h.runSteps(t, ref, 1+NoProgressLimit)

	st := h.status(t, ref)
	if st.Phase != PhaseStalled {
		t.Fatalf("phase = %q, want %q", st.Phase, PhaseStalled)
	}
	if !strings.Contains(st.Message, "no progress") {
		t.Fatalf("stall message is not a no-progress reason: %q", st.Message)
	}
	if !strings.Contains(st.Message, "re-reading the same files") {
		t.Fatalf("stall message does not name the last activity: %q", st.Message)
	}
	if reason := stalledReason(st); reason != "NoProgress" {
		t.Fatalf("stalled condition reason = %q, want NoProgress", reason)
	}
	// It stalled for lack of progress, not for the budget: far fewer than MaxSteps ran.
	if st.Steps >= 50 {
		t.Fatalf("goal ran out its budget (%d steps) instead of stopping for no progress", st.Steps)
	}
}

// TestReconcilerWarnsBeforeStopping: the step before the limit carries the stalling nudge
// for the next step, so the agent is told it is stalling and gets a chance to change
// course before it is stopped.
func TestReconcilerWarnsBeforeStopping(t *testing.T) {
	probe := &fakeProbe{fingerprints: []string{"idle"}, summary: "spinning"}
	h := newHarness(t, neverStop{}, WithProgressProbe(probe))
	ref := h.createGoal(t, "warned", Spec{Objective: "loop", StopCondition: "never", MaxSteps: 50})

	// Baseline + (warn-point) idle steps: the streak is now at progressWarnAt.
	h.runSteps(t, ref, 1+progressWarnAt)

	st := h.status(t, ref)
	if st.Phase == PhaseStalled {
		t.Fatal("stopped at the warn point instead of warning first")
	}
	if st.ProgressNudge == "" || !strings.Contains(st.ProgressNudge, "will stop") {
		t.Fatalf("no stalling nudge stamped for the next step: %q", st.ProgressNudge)
	}
}

// TestReconcilerDoesNotStallWhenMakingProgress: a goal whose probe reports a fresh
// fingerprint every step never trips detection, even past the limit; it settles for its
// own reason (convergence) instead.
func TestReconcilerDoesNotStallWhenMakingProgress(t *testing.T) {
	probe := &fakeProbe{fingerprints: []string{"a", "b", "c", "d", "e", "f", "g"}, summary: "real work"}
	h := newHarness(t, stopAfter{at: 5}, WithProgressProbe(probe))
	ref := h.createGoal(t, "productive", Spec{Objective: "do real work", StopCondition: "done", MaxSteps: 50})

	// Drive well past NoProgressLimit; each step is genuinely different.
	for range 6 {
		h.reconcile(t, ref)
		if st := h.status(t, ref); st.Phase == PhaseConverged || st.Phase == PhaseStalled {
			break
		}
		h.completeStep(t)
		h.reconcile(t, ref)
	}

	st := h.status(t, ref)
	if st.Phase == PhaseStalled {
		t.Fatalf("a goal making real progress was stalled: %q", st.Message)
	}
	if st.Phase != PhaseConverged {
		t.Fatalf("phase = %q, want %q", st.Phase, PhaseConverged)
	}
}

// TestNoProgressDisabledWithoutAProbe: a reconciler with no probe wired does not run
// detection at all — a goal is bounded only by its budget, exactly as before the feature.
func TestNoProgressDisabledWithoutAProbe(t *testing.T) {
	h := newHarness(t, neverStop{}) // no WithProgressProbe
	ref := h.createGoal(t, "unwatched", Spec{Objective: "loop", StopCondition: "never", MaxSteps: 4})

	h.runSteps(t, ref, 4)

	st := h.status(t, ref)
	if st.IdleStreak != 0 {
		t.Fatalf("idle streak advanced with no probe wired: %d", st.IdleStreak)
	}
	if !strings.Contains(st.Message, "budget") {
		t.Fatalf("without a probe a looping goal should stall on budget, got: %q", st.Message)
	}
}

// TestReconcilerPropagatesATransientProbeError: a probe that fails to read the record for
// a moment is a transient error the reconcile returns (to be retried), not a stall — a
// blip in reading the record must not be mistaken for the run making no progress.
func TestReconcilerPropagatesATransientProbeError(t *testing.T) {
	probe := &fakeProbe{err: fault.New(fault.Transient, "probe_read", "spine blip")}
	h := newHarness(t, neverStop{}, WithProgressProbe(probe))
	ref := h.createGoal(t, "probe-blip", Spec{Objective: "x", StopCondition: "never", MaxSteps: 50})

	h.reconcile(t, ref) // dispatch a build step
	h.completeStep(t)
	_, err := h.gr.Reconcile(h.ctx, ref) // observe -> probe errors transiently
	if err == nil {
		t.Fatal("a transient probe error was not propagated (it would be retried)")
	}
	if st := h.status(t, ref); st.Phase == PhaseStalled {
		t.Fatalf("a transient probe error stalled the goal: %q", st.Message)
	}
}

// stalledReason returns the reason of the Stalled condition, or "".
func stalledReason(s Status) string {
	for _, c := range s.Conditions {
		if c.Type == CondStalled && c.Status == "True" {
			return c.Reason
		}
	}
	return ""
}

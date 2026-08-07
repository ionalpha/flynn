package goal

import (
	"context"
	"strconv"
	"strings"
	"testing"
)

// --- fakes ------------------------------------------------------------------

// refusing never converges and always says the same thing about why, which is the run
// this guard exists to catch: busy, spending, and told nothing new.
type refusing struct{ reason string }

func (s refusing) Met(context.Context, Spec, Status) (bool, string, error) {
	return false, s.reason, nil
}

// rewording never converges and says something different every time. It is the goal that
// must survive: the refusal changing is the run being told something new, however slowly.
type rewording struct{ calls int }

func (s *rewording) Met(context.Context, Spec, Status) (bool, string, error) {
	s.calls++
	return false, "still failing on case " + strconv.Itoa(s.calls), nil
}

// --- the counter ------------------------------------------------------------

// TestObserveVerdictCountsConsecutiveRefusals: the count starts at one on a refusal with
// new substance and rises while the substance holds, so the limit reads as the number of
// cycles that ended the same way.
func TestObserveVerdictCountsConsecutiveRefusals(t *testing.T) {
	var s Status
	if n := s.ObserveVerdict("the health check still returns 500", ""); n != 1 {
		t.Fatalf("first refusal counted %d, want 1", n)
	}
	if s.StalledForNonConvergence() {
		t.Fatal("stalled on the first refusal, which is information rather than repetition")
	}
	if n := s.ObserveVerdict("the health check still returns 500", ""); n != VerdictRepeatLimit {
		t.Fatalf("second identical refusal counted %d, want %d", n, VerdictRepeatLimit)
	}
	if !s.StalledForNonConvergence() {
		t.Fatalf("did not stall at %d identical refusals (limit %d)", s.VerdictRepeat, VerdictRepeatLimit)
	}
}

// TestObserveVerdictResetsOnADifferentRefusal: a refusal whose substance changed is the
// run being told something new, so the count starts over rather than accumulating across
// unrelated complaints.
func TestObserveVerdictResetsOnADifferentRefusal(t *testing.T) {
	var s Status
	s.ObserveVerdict("the endpoint is missing", "")
	s.ObserveVerdict("the endpoint is missing", "")
	if !s.StalledForNonConvergence() {
		t.Fatal("two identical refusals did not reach the limit")
	}
	if n := s.ObserveVerdict("the endpoint returns the wrong shape", ""); n != 1 {
		t.Fatalf("a new refusal counted %d, want the count to start over at 1", n)
	}
	if s.StalledForNonConvergence() {
		t.Fatal("still reads as stalled after being told something new")
	}
}

// TestObserveVerdictComparesTheItemDetailToo: under the ledger gate the refusal a run
// actually receives is its current item's failed check, so a change there is a change in
// what the run was told even when the evaluator's own wording never moves.
func TestObserveVerdictComparesTheItemDetailToo(t *testing.T) {
	var s Status
	s.ObserveVerdict("", "go test ./api/...: 3 tests failing")
	if n := s.ObserveVerdict("", "go test ./api/...: 2 tests failing"); n != 1 {
		t.Fatalf("a changed check detail counted %d, want 1", n)
	}
	if n := s.ObserveVerdict("", "go test ./api/...: 2 tests failing"); n != 2 {
		t.Fatalf("an unchanged check detail counted %d, want 2", n)
	}
}

// TestProvenLedgerItemResetsTheCount is the fixture the whole design has to pass: a run
// doing real work, one ledger item at a time, whose evaluator keeps giving the same
// summary because the goal as a whole genuinely is not done yet. Proving an item is the
// run advancing against its own definition of done, and it must clear the count no matter
// how the refusal is worded.
func TestProvenLedgerItemResetsTheCount(t *testing.T) {
	ledger := twoItemLedger(t)
	var s Status
	s.SyncLedger(ledger)

	s.ObserveVerdict("not all of the plan is done", "")
	if err := s.MarkProven(ledger[0].ID, "1", testNow); err != nil {
		t.Fatal(err)
	}
	if n := s.ObserveVerdict("not all of the plan is done", ""); n != 1 {
		t.Fatalf("the cycle that proved an item counted %d, want the count to start over at 1", n)
	}
	if s.StalledForNonConvergence() {
		t.Fatal("stopped a run that had just proven a ledger item")
	}
}

// TestObserveVerdictIgnoresASilentRefusal: an evaluator that declines without saying why
// has given nothing to compare. Counting silence as repetition would stop every goal
// driven by an evaluator that returns a bare false, which is what Flynn's own shipped
// evaluators do.
func TestObserveVerdictIgnoresASilentRefusal(t *testing.T) {
	var s Status
	for range VerdictRepeatLimit + 3 {
		if n := s.ObserveVerdict("", ""); n != 0 {
			t.Fatalf("a silent refusal counted %d, want 0", n)
		}
	}
	if s.StalledForNonConvergence() {
		t.Fatal("stalled a goal whose evaluator never stated a reason")
	}
}

// TestNormalizeVerdictIgnoresWordingButNotNumbers: case, punctuation and spacing carry no
// meaning, so a rephrased complaint is still the same complaint. Digits do carry meaning:
// a count that moved is the clearest evidence of progress a refusal can contain, and
// treating it as noise would stop a run that was converging.
func TestNormalizeVerdictIgnoresWordingButNotNumbers(t *testing.T) {
	same := []struct{ a, b string }{
		{"Tests still failing.", "tests   still failing"},
		{"the API returns 500", "The API -- returns 500!"},
	}
	for _, c := range same {
		if normalizeVerdict(c.a) != normalizeVerdict(c.b) {
			t.Fatalf("%q and %q normalized apart: %q vs %q", c.a, c.b, normalizeVerdict(c.a), normalizeVerdict(c.b))
		}
	}
	if normalizeVerdict("3 tests failing") == normalizeVerdict("2 tests failing") {
		t.Fatal("a changed count normalized to the same substance, which would hide progress")
	}
}

// TestNonConvergenceReasonQuotesTheRefusal: the stall has to say what the run kept being
// told, because that sentence is the only thing the run established and it is what an
// operator needs in order to answer it.
func TestNonConvergenceReasonQuotesTheRefusal(t *testing.T) {
	var s Status
	s.ObserveVerdict("the deploy target does not exist", "")
	s.ObserveVerdict("the deploy target does not exist", "")

	msg := s.NonConvergenceReason()
	if !strings.Contains(msg, "not converging") {
		t.Fatalf("reason is not a non-convergence message: %q", msg)
	}
	if !strings.Contains(msg, "the deploy target does not exist") {
		t.Fatalf("reason does not quote what the run was told: %q", msg)
	}
	if !strings.Contains(msg, "?") {
		t.Fatalf("reason does not ask the operator anything: %q", msg)
	}

	// Under the ledger gate both halves are usually present, and the stall has to carry
	// both: the evaluator's summary alone does not say which check kept failing.
	var gated Status
	gated.ObserveVerdict("the plan is not finished", "curl --fail localhost/health: connection refused")
	both := gated.NonConvergenceReason()
	if !strings.Contains(both, "the plan is not finished") || !strings.Contains(both, "connection refused") {
		t.Fatalf("reason dropped one half of what the run was told: %q", both)
	}
}

// --- reconciler integration -------------------------------------------------

// TestReconcilerStopsANonConvergingGoal: a run whose evaluator refuses for the same
// reason cycle after cycle stops with that reason, and stops far short of the budget it
// would otherwise have spent discovering the same thing.
func TestReconcilerStopsANonConvergingGoal(t *testing.T) {
	h := newHarness(t, refusing{"the deploy target does not exist"})
	ref := h.createGoal(t, "unsatisfiable", Spec{Objective: "deploy it", StopCondition: "deployed", MaxSteps: 50})

	h.runSteps(t, ref, VerdictRepeatLimit)

	st := h.status(t, ref)
	if st.Phase != PhaseStalled {
		t.Fatalf("phase = %q, want %q", st.Phase, PhaseStalled)
	}
	if reason := stalledReason(st); reason != "NotConverging" {
		t.Fatalf("stalled condition reason = %q, want NotConverging", reason)
	}
	if !strings.Contains(st.Message, "the deploy target does not exist") {
		t.Fatalf("stall message does not name what it kept being told: %q", st.Message)
	}
	if st.Steps >= 50 {
		t.Fatalf("ran out its budget (%d steps) instead of stopping for non-convergence", st.Steps)
	}
}

// TestReconcilerKeepsGoingWhileTheRefusalChanges: the run that must survive. Its evaluator
// refuses every cycle, but says something different each time, so the run is being told
// something new and has not stopped converging.
func TestReconcilerKeepsGoingWhileTheRefusalChanges(t *testing.T) {
	h := newHarness(t, &rewording{})
	ref := h.createGoal(t, "converging", Spec{Objective: "fix them", StopCondition: "all pass", MaxSteps: 50})

	h.runSteps(t, ref, VerdictRepeatLimit+3)

	st := h.status(t, ref)
	if st.Phase == PhaseStalled {
		t.Fatalf("stopped a run that was told something new every cycle: %q", st.Message)
	}
	if st.VerdictRepeat != 1 {
		t.Fatalf("repeat count = %d, want 1: every cycle changed the refusal", st.VerdictRepeat)
	}
}

// TestSilentEvaluatorReachesItsBudgetInstead: a goal driven by an evaluator that states no
// reason is untouched by this guard and stops for its budget exactly as it did before, so
// turning the signal on does not change any run that has nothing to signal with.
func TestSilentEvaluatorReachesItsBudgetInstead(t *testing.T) {
	h := newHarness(t, neverStop{})
	ref := h.createGoal(t, "silent", Spec{Objective: "loop", StopCondition: "never", MaxSteps: 3})

	h.runSteps(t, ref, 3)

	st := h.status(t, ref)
	if reason := stalledReason(st); reason != "BudgetExhausted" {
		t.Fatalf("stalled condition reason = %q, want BudgetExhausted", reason)
	}
	if st.VerdictRepeat != 0 {
		t.Fatalf("counted %d refusals from an evaluator that stated none", st.VerdictRepeat)
	}
}

// TestProvingItemsHoldsOffTheGuard is the fixture at the level it actually runs: a real
// build-and-check round trip through the ledger loop, with the evaluator saying the same
// thing every cycle because the plan as a whole is not finished. Each cycle proves an
// item, so the run is advancing and must not be stopped.
func TestProvingItemsHoldsOffTheGuard(t *testing.T) {
	ev := &fakeEvidence{}
	ver := &fakeVerifier{verdicts: []ItemVerdict{{Passed: true, Executed: true, Detail: "check passed"}}}
	h := newHarness(t, refusing{"the plan is not finished"}, WithLedgerGate(ev, newGate(t, RequireExecuted())))
	w := NewWorker(h.store, h.jobs, h.clk, &fakeExec{}, WithItemVerification(ver, ev))

	ledger := twoItemLedger(t)
	ref := h.plannedGoal(t, "proving", ledger)

	// Two full cycles: build, then that item's declared check. Each one proves an item.
	for range 2 {
		h.reconcile(t, ref) // dispatch the build step
		if _, err := w.ProcessOnce(h.ctx); err != nil {
			t.Fatalf("build step: %v", err)
		}
		h.reconcile(t, ref) // observe it, dispatch the item's check
		if _, err := w.ProcessOnce(h.ctx); err != nil {
			t.Fatalf("verify step: %v", err)
		}
		h.reconcile(t, ref) // observe the check and settle the ledger
	}

	st := h.status(t, ref)
	if st.Phase == PhaseStalled {
		t.Fatalf("stopped a run that proved an item every cycle: %q", st.Message)
	}
	if n := st.provenCount(); n != 2 {
		t.Fatalf("proven items = %d, want 2: the fixture did not do the work it claims to", n)
	}
	if st.VerdictRepeat > 1 {
		t.Fatalf("repeat count = %d, want at most 1: each cycle advanced the ledger", st.VerdictRepeat)
	}
}

// TestAnUnrunnableCheckIsNotARefusal: a check that could not be executed reports on the
// host rather than on the work. On a machine that cannot contain a model-authored command
// every check comes back unrunnable with identical wording forever, so counting it would
// stop every goal on that host for the one reason that says nothing about whether it was
// converging.
func TestAnUnrunnableCheckIsNotARefusal(t *testing.T) {
	ledger := twoItemLedger(t)
	var s Status
	s.SyncLedger(ledger)
	s.ItemFeedback = "the check could not be run: no kernel containment on this host"

	unrun := []Verification{{Ref: "1", Item: ledger[0].ID, Passed: false, Provenance: ProvenanceAsserted}}
	if got := s.ExecutedFeedback(ledger, unrun); got != "" {
		t.Fatalf("an unrunnable check offered %q as a refusal to count", got)
	}
	for range VerdictRepeatLimit + 2 {
		s.ObserveVerdict("", s.ExecutedFeedback(ledger, unrun))
	}
	if s.StalledForNonConvergence() {
		t.Fatal("stopped a run because its checks could not run, which is a host problem")
	}

	// The same detail behind a check that did run is exactly what must be counted.
	ran := []Verification{{Ref: "2", Item: ledger[0].ID, Passed: false, Provenance: ProvenanceExecuted}}
	if got := s.ExecutedFeedback(ledger, ran); got != s.ItemFeedback {
		t.Fatalf("an executed check's detail was dropped: %q", got)
	}
}

// TestExecutedFeedbackWithNothingToReportOn: no feedback, no current item, or no recorded
// verification for it all mean there is no refusal to count.
func TestExecutedFeedbackWithNothingToReportOn(t *testing.T) {
	ledger := twoItemLedger(t)
	var s Status
	s.SyncLedger(ledger)

	if got := s.ExecutedFeedback(ledger, nil); got != "" {
		t.Fatalf("empty feedback produced %q", got)
	}
	s.ItemFeedback = "something failed"
	if got := s.ExecutedFeedback(ledger, nil); got != "" {
		t.Fatalf("feedback with no recorded verification behind it produced %q", got)
	}
	if got := s.ExecutedFeedback(nil, nil); got != "" {
		t.Fatalf("feedback with no current item produced %q", got)
	}
}

// TestPlanningIsNotACycle: the refusal standing before any work has been attempted says
// nothing about whether attempting it will help, so the planning step does not count
// toward repetition and a planned goal is not stopped a cycle early.
func TestPlanningIsNotACycle(t *testing.T) {
	if (&Reconciler{}).countsAsCycle(true, PlanJobKind, Status{}) {
		t.Fatal("a completed planning step counted as a cycle")
	}
	if (&Reconciler{}).countsAsCycle(false, StepJobKind, Status{}) {
		t.Fatal("a pass that observed no completed job counted as a cycle")
	}
	if (&Reconciler{}).countsAsCycle(true, StepJobKind, Status{VerifyPending: true}) {
		t.Fatal("a build step whose check has not run yet counted as a complete cycle")
	}
	if !(&Reconciler{}).countsAsCycle(true, VerifyJobKind, Status{}) {
		t.Fatal("the check that closed the cycle did not count as one")
	}
}

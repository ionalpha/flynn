package goal

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/chain"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/jobs"
	"github.com/ionalpha/flynn/reconcile"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/spine"
)

// --- fakes ------------------------------------------------------------------

// fakeEvidence is an in-memory goal.Evidence: it appends verifications with
// monotonically increasing refs, the same identity discipline the spine gives the real
// one, and hands them all back on Recorded.
type fakeEvidence struct {
	recs     []Verification
	readErr  error
	writeErr error
}

func (f *fakeEvidence) Record(_ context.Context, _ resource.Resource, item string, v ItemVerdict) (Verification, error) {
	if f.writeErr != nil {
		return Verification{}, f.writeErr
	}
	prov := ProvenanceAsserted
	if v.Executed {
		prov = ProvenanceExecuted
	}
	rec := Verification{Ref: strconv.Itoa(len(f.recs) + 1), Item: item, Passed: v.Passed, Provenance: prov}
	f.recs = append(f.recs, rec)
	return rec, nil
}

func (f *fakeEvidence) Recorded(context.Context, resource.Resource) ([]Verification, error) {
	if f.readErr != nil {
		return nil, f.readErr
	}
	return f.recs, nil
}

// fakeVerifier returns a scripted verdict per call and records which items it was asked
// about, so a test can assert the producer was pointed at the run's current item.
type fakeVerifier struct {
	verdicts []ItemVerdict
	asked    []string
	err      error
}

func (f *fakeVerifier) VerifyItem(_ context.Context, _ resource.Resource, item LedgerItem) (ItemVerdict, error) {
	f.asked = append(f.asked, item.ID)
	if f.err != nil {
		return ItemVerdict{}, f.err
	}
	if len(f.verdicts) == 0 {
		return ItemVerdict{}, nil
	}
	v := f.verdicts[0]
	if len(f.verdicts) > 1 {
		f.verdicts = f.verdicts[1:]
	}
	return v, nil
}

// alwaysMet is a stop evaluator that reports the model has finished on every pass: the
// exact pressure the ledger gate exists to hold a claim against.
type alwaysMet struct{}

func (alwaysMet) Met(context.Context, Spec, Status) (bool, string, error) {
	return true, "the model says it is done", nil
}

// neverMet is a stop evaluator that never converges, so a test drives the run loop
// itself rather than the completion path.
type neverMet struct{}

func (neverMet) Met(context.Context, Spec, Status) (bool, string, error) { return false, "", nil }

// --- helpers ----------------------------------------------------------------

// twoItemLedger builds a ledger with two content-addressed items.
func twoItemLedger(t *testing.T) []LedgerItem {
	t.Helper()
	ledger, err := AppendItems(nil,
		LedgerItem{Item: "add the endpoint", Verify: "curl --fail localhost/health"},
		LedgerItem{Item: "cover it with a test", Verify: "go test ./api/..."},
	)
	if err != nil {
		t.Fatalf("build ledger: %v", err)
	}
	return ledger
}

// plannedGoal creates a goal carrying ledger and already marked planned, which is the
// state the planning phase leaves behind and the state every ledger-gate branch is about.
func (h *harness) plannedGoal(t *testing.T, name string, ledger []LedgerItem) reconcile.Ref {
	t.Helper()
	ref := h.createGoal(t, name, Spec{Objective: "o", StopCondition: "c", Ledger: ledger})
	h.setStatus(t, ref, func(st *Status) {
		st.Planned = true
		st.SyncLedger(ledger)
	})
	return ref
}

// setStatus applies mutate to the goal's stored status, so a test can start from the
// state an earlier phase would have left rather than driving that phase first.
func (h *harness) setStatus(t *testing.T, ref reconcile.Ref, mutate func(*Status)) {
	t.Helper()
	r, err := h.store.Get(h.ctx, ref.Kind, ref.Scope, ref.Name)
	if err != nil {
		t.Fatalf("get goal: %v", err)
	}
	st, err := DecodeStatus(r)
	if err != nil {
		t.Fatal(err)
	}
	mutate(&st)
	enc, err := st.Encode()
	if err != nil {
		t.Fatal(err)
	}
	r.Status = enc
	if _, err := h.store.Put(h.ctx, r); err != nil {
		t.Fatalf("put goal: %v", err)
	}
}

// completeJob claims the queued job, asserts it is the kind the test expects, and
// completes it: the worker's half of one dispatch-and-observe round.
func (h *harness) completeJob(t *testing.T, kind string) {
	t.Helper()
	claimed, err := h.jobs.Claim(h.ctx, jobs.ClaimParams{Queue: StepQueue, Limit: 1, LeaseFor: int64(time.Minute)})
	if err != nil || len(claimed) == 0 {
		t.Fatalf("no job to claim (err=%v)", err)
	}
	if claimed[0].Kind != kind {
		t.Fatalf("claimed job kind = %q, want %q", claimed[0].Kind, kind)
	}
	if err := h.jobs.Complete(h.ctx, claimed[0].ID); err != nil {
		t.Fatalf("complete job: %v", err)
	}
}

// producerHarness assembles a reconciler with the ledger loop closed and a worker that
// shares its record, so a test drives the real dispatch-and-observe round trip. Passing a
// nil verifier builds the worker without a producer, which is the misconfiguration the
// loud-failure test is about.
func producerHarness(t *testing.T, ver ItemVerifier, ev Evidence) (*harness, *Worker) {
	t.Helper()
	h := newHarness(t, neverMet{}, WithLedgerGate(ev, newGate(t, RequireExecuted())))
	wopts := []WorkerOption{}
	if ver != nil {
		wopts = append(wopts, WithItemVerification(ver, ev))
	}
	return h, NewWorker(h.store, h.jobs, h.clk, &fakeExec{}, wopts...)
}

// newGate builds a gate for a test and fails the test if construction (and thus the
// self-test) does not succeed, so every gate test starts from a gate that has just
// proved itself.
func newGate(t *testing.T, opts ...GateOption) *EvidenceGate {
	t.Helper()
	g, err := NewEvidenceGate(opts...)
	if err != nil {
		t.Fatalf("NewEvidenceGate: %v", err)
	}
	return g
}

// --- selection --------------------------------------------------------------

// TestCurrentItemIsFirstUnproven: the run's current item is derived from the ledger and
// the per-item state, not stored, so there is no second representation of where the run
// is that could drift from the record.
func TestCurrentItemIsFirstUnproven(t *testing.T) {
	ledger := twoItemLedger(t)
	var st Status
	st.SyncLedger(ledger)

	item, ok := st.CurrentItem(ledger)
	if !ok || item.ID != ledger[0].ID {
		t.Fatalf("current item = %+v (ok=%v), want the first item", item, ok)
	}

	if err := st.MarkProven(ledger[0].ID, "1", testNow); err != nil {
		t.Fatal(err)
	}
	item, ok = st.CurrentItem(ledger)
	if !ok || item.ID != ledger[1].ID {
		t.Fatalf("after proving item 1, current = %+v (ok=%v), want item 2", item, ok)
	}

	if err := st.MarkProven(ledger[1].ID, "2", testNow); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.CurrentItem(ledger); ok {
		t.Fatal("a fully proven ledger still reported a current item")
	}
	if _, ok := (Status{}).CurrentItem(nil); ok {
		t.Fatal("an empty ledger reported a current item")
	}
}

// --- provenance -------------------------------------------------------------

// TestProvenanceReadsUnknownValuesAsAsserted: default-FAIL applied to the second axis. An
// event with a verdict but no readable provenance carries everything the original contract
// had, so it is not skipped, but it is taken at its weakest reading, because the only
// thing that may buy an event the executed kind is that exact value, present and readable.
func TestProvenanceReadsUnknownValuesAsAsserted(t *testing.T) {
	base := map[string]any{chain.ItemKey: "abc", chain.ItemPassedKey: true}
	cases := []struct {
		name string
		prov any
		want Provenance
	}{
		{"absent", nil, ProvenanceAsserted},
		{"asserted", chain.ProvenanceAsserted, ProvenanceAsserted},
		{"executed", chain.ProvenanceExecuted, ProvenanceExecuted},
		{"not a string", 7, ProvenanceAsserted},
		{"a value this build does not know", "attested-by-a-future-scheme", ProvenanceAsserted},
		{"a near miss", "Executed", ProvenanceAsserted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := map[string]any{}
			for k, v := range base {
				payload[k] = v
			}
			if tc.prov != nil {
				payload[chain.ItemProvenanceKey] = tc.prov
			}
			got := VerificationsFrom([]spine.Event{{Seq: 3, Type: chain.ItemVerified, Payload: payload}})
			if len(got) != 1 {
				t.Fatalf("read %d verifications, want 1", len(got))
			}
			if got[0].Provenance != tc.want {
				t.Fatalf("provenance = %q, want %q", got[0].Provenance, tc.want)
			}
			if got[0].Ref != "3" {
				t.Fatalf("ref = %q, want the event sequence", got[0].Ref)
			}
		})
	}
}

// TestGateRequiringExecutionRefusesAnAssertion: the invariant that makes the provenance
// axis decide something rather than label it. There is no promotion and no override, so an
// unrunnable check fails its item instead of quietly passing it.
func TestGateRequiringExecutionRefusesAnAssertion(t *testing.T) {
	const item = "item0000000000aa"
	asserted := []Verification{{Ref: "1", Item: item, Passed: true, Provenance: ProvenanceAsserted}}
	executed := []Verification{{Ref: "2", Item: item, Passed: true, Provenance: ProvenanceExecuted}}

	strict := newGate(t, RequireExecuted())
	if _, err := strict.admit(item, asserted, nil); !errors.Is(err, ErrEvidenceAsserted) {
		t.Fatalf("an assertion satisfied a gate requiring execution: %v", err)
	}
	if _, err := strict.admit(item, executed, nil); err != nil {
		t.Fatalf("an executed check was refused by a strict gate: %v", err)
	}

	// A gate that was not configured for it must still admit an assertion, so turning
	// the policy on stays a decision rather than something the gate does on its own.
	lax := newGate(t)
	if _, err := lax.admit(item, asserted, nil); err != nil {
		t.Fatalf("a permissive gate refused an assertion: %v", err)
	}
}

// TestGateSelfTestCatchesADeletedProvenanceRefusal: the self-test is what makes the gate
// unable to ship broken, so it has to catch the provenance branch going missing too. A
// gate whose execution requirement had been refactored away would otherwise construct
// cleanly and wave assertions through at runtime.
func TestGateSelfTestCatchesADeletedProvenanceRefusal(t *testing.T) {
	// A decision that enforces every original rule but ignores provenance entirely.
	ignoresProvenance := func(itemID string, recorded []Verification, consumed map[string]bool) (string, error) {
		g := &EvidenceGate{} // requireExecuted deliberately off
		return g.admit(itemID, recorded, consumed)
	}
	if err := selfTest(ignoresProvenance, true); !errors.Is(err, ErrGateBroken) {
		t.Fatalf("selfTest passed a gate that ignores its execution requirement: %v", err)
	}
	if err := selfTest(ignoresProvenance, false); err != nil {
		t.Fatalf("selfTest failed a correct permissive gate: %v", err)
	}
}

// --- settling ---------------------------------------------------------------

// TestProveRecordedSettlesFromTheRecordAndSpendsOnce: an item flips to proven only from a
// verification on the record, and that verification is spent, so one recorded check cannot
// certify two items.
func TestProveRecordedSettlesFromTheRecordAndSpendsOnce(t *testing.T) {
	ledger := twoItemLedger(t)
	var st Status
	st.SyncLedger(ledger)
	gate := newGate(t, RequireExecuted())
	now := testNow

	if n := st.ProveRecorded(gate, nil, now); n != 0 {
		t.Fatalf("proved %d items from an empty record, want 0", n)
	}

	recorded := []Verification{{Ref: "9", Item: ledger[0].ID, Passed: true, Provenance: ProvenanceExecuted}}
	if n := st.ProveRecorded(gate, recorded, now); n != 1 {
		t.Fatalf("proved %d items, want 1", n)
	}
	if st.Ledger[0].Evidence != "9" {
		t.Fatalf("item 1 evidence = %q, want the verification ref", st.Ledger[0].Evidence)
	}
	if st.LedgerSettled() {
		t.Fatal("a ledger with one item still unproven reported settled")
	}

	// A second pass over the same record must not re-spend the consumed verification on
	// the remaining item, whose id it does not name in any case.
	if n := st.ProveRecorded(gate, recorded, now); n != 0 {
		t.Fatalf("a repeat settling pass proved %d more items, want 0", n)
	}

	recorded = append(recorded, Verification{Ref: "10", Item: ledger[1].ID, Passed: true, Provenance: ProvenanceExecuted})
	if n := st.ProveRecorded(gate, recorded, now); n != 1 {
		t.Fatalf("proved %d items on the second check, want 1", n)
	}
	if !st.LedgerSettled() {
		t.Fatal("a ledger with every item proven did not report settled")
	}
}

// TestUnprovenReasonsDistinguishTheThreeRefusals: a run record that flattened these into
// "not done" would lose the difference between an item nobody checked, one whose check was
// spent elsewhere, and one whose check could not be run: three problems with three fixes.
func TestUnprovenReasonsDistinguishTheThreeRefusals(t *testing.T) {
	ledger, err := AppendItems(nil,
		LedgerItem{Item: "nothing checked this", Verify: "true"},
		LedgerItem{Item: "its check was spent", Verify: "false"},
		LedgerItem{Item: "its check was only claimed", Verify: "echo hi"},
		LedgerItem{Item: "already done", Verify: "test -f go.mod"},
	)
	if err != nil {
		t.Fatal(err)
	}
	var st Status
	st.SyncLedger(ledger)
	// The settled item consumed ref "5", which is also the only passing verification the
	// second item has: that is what a spent refusal looks like from the record's side.
	if err := st.MarkProven(ledger[3].ID, "5", testNow); err != nil {
		t.Fatal(err)
	}
	recorded := []Verification{
		{Ref: "5", Item: ledger[1].ID, Passed: true, Provenance: ProvenanceExecuted},
		{Ref: "6", Item: ledger[2].ID, Passed: true, Provenance: ProvenanceAsserted},
	}

	reasons := st.UnprovenReasons(newGate(t, RequireExecuted()), recorded)
	if len(reasons) != 3 {
		t.Fatalf("got %d reasons, want one per unproven item: %v", len(reasons), reasons)
	}
	want := []string{
		"no recorded passing verification",
		"already consumed by another item",
		"asserted, not executed",
	}
	for i, w := range want {
		if !strings.Contains(reasons[i], w) {
			t.Fatalf("reason %d = %q, want it to mention %q", i, reasons[i], w)
		}
		if !strings.HasPrefix(reasons[i], ledger[i].ID) {
			t.Fatalf("reason %d = %q, want it to name item %s", i, reasons[i], ledger[i].ID)
		}
	}
	if (Status{}).UnprovenReasons(newGate(t), nil) != nil {
		t.Fatal("a goal with no ledger produced unproven reasons")
	}
}

// --- the loop ---------------------------------------------------------------

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

// --- the producer -----------------------------------------------------------

// TestVerifyStepRecordsTheVerdictAndProvesNothing: the producer writes to the record and
// stops. Proving is the reconciler's, from the record, so there is no path from "a check
// ran" to "an item is done" that skips the gate, and a crash between the two costs
// nothing, because the next reconcile settles what the record already holds.
func TestVerifyStepRecordsTheVerdictAndProvesNothing(t *testing.T) {
	ev := &fakeEvidence{}
	ver := &fakeVerifier{verdicts: []ItemVerdict{{Passed: false, Executed: true, Detail: "exit 1: two tests failed"}}}
	h, w := producerHarness(t, ver, ev)

	ledger := twoItemLedger(t)
	ref := h.plannedGoal(t, "produce", ledger)
	h.reconcile(t, ref) // dispatches the build step
	if _, err := w.ProcessOnce(h.ctx); err != nil {
		t.Fatalf("build step: %v", err)
	}
	h.reconcile(t, ref) // observes it and dispatches the item's check

	processed, err := w.ProcessOnce(h.ctx)
	if err != nil || !processed {
		t.Fatalf("ProcessOnce = (%v, %v), want a verify step to run", processed, err)
	}
	if len(ver.asked) != 1 || ver.asked[0] != ledger[0].ID {
		t.Fatalf("verifier was asked about %v, want only the run's current item", ver.asked)
	}
	if len(ev.recs) != 1 || ev.recs[0].Item != ledger[0].ID || ev.recs[0].Passed {
		t.Fatalf("recorded %+v, want one failing verdict for the current item", ev.recs)
	}
	if ev.recs[0].Provenance != ProvenanceExecuted {
		t.Fatalf("provenance = %q, want the verdict's own executed marker", ev.recs[0].Provenance)
	}
	st := h.status(t, ref)
	if st.Ledger[0].Proven {
		t.Fatal("the producer marked an item proven; only the reconciler may do that")
	}
	if st.ItemFeedback != "exit 1: two tests failed" {
		t.Fatalf("item feedback = %q, want the failing check's detail", st.ItemFeedback)
	}
}

// TestVerifyStepFailsLoudlyWithNoProducer: a goal gated on its ledger by a reconciler
// whose worker cannot produce evidence would refuse to converge forever with nothing on
// the record saying why, which is the silent-gate failure one level up.
func TestVerifyStepFailsLoudlyWithNoProducer(t *testing.T) {
	h, w := producerHarness(t, nil, &fakeEvidence{})
	ref := h.plannedGoal(t, "noproducer", twoItemLedger(t))
	r, err := h.store.Get(h.ctx, ref.Kind, ref.Scope, ref.Name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.jobs.Enqueue(h.ctx, jobs.EnqueueParams{Queue: StepQueue, Kind: VerifyJobKind, Payload: []byte(r.ID)}); err != nil {
		t.Fatal(err)
	}

	if _, err := w.ProcessOnce(h.ctx); err != nil {
		t.Fatalf("ProcessOnce: %v", err)
	}
	claimed, err := h.jobs.Claim(h.ctx, jobs.ClaimParams{Queue: StepQueue, Limit: 5, LeaseFor: int64(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 0 {
		t.Fatal("a verify step with no producer was left retryable; a missing port is not fixed by retrying")
	}
}

// TestVerifyStepWithNothingLeftToCheckCompletes: a verification that arrives after the
// item it was for has settled is a job that outlived its purpose, not a failure.
func TestVerifyStepWithNothingLeftToCheckCompletes(t *testing.T) {
	ver := &fakeVerifier{}
	h, w := producerHarness(t, ver, &fakeEvidence{})

	ledger := twoItemLedger(t)
	ref := h.plannedGoal(t, "settled", ledger)
	h.setStatus(t, ref, func(st *Status) {
		for i := range st.Ledger {
			if err := st.MarkProven(st.Ledger[i].ID, "x", testNow); err != nil {
				t.Fatal(err)
			}
		}
	})
	r, err := h.store.Get(h.ctx, ref.Kind, ref.Scope, ref.Name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.jobs.Enqueue(h.ctx, jobs.EnqueueParams{Queue: StepQueue, Kind: VerifyJobKind, Payload: []byte(r.ID)}); err != nil {
		t.Fatal(err)
	}

	if _, err := w.ProcessOnce(h.ctx); err != nil {
		t.Fatalf("ProcessOnce: %v", err)
	}
	if len(ver.asked) != 0 {
		t.Fatalf("verifier ran against %v, want nothing on a settled ledger", ver.asked)
	}
}

// TestALostVerdictFailsTheJob: a verdict that fails to land is a check that ran for
// nothing (the item stays unproven with no record of why and the work repeats), so the
// write is not best-effort the way a checkpoint is.
func TestALostVerdictFailsTheJob(t *testing.T) {
	ev := &fakeEvidence{writeErr: fault.New(fault.Terminal, "evidence_append", "the record rejected the write")}
	ver := &fakeVerifier{verdicts: []ItemVerdict{{Passed: true, Executed: true}}}
	h, w := producerHarness(t, ver, ev)

	ref := h.plannedGoal(t, "lost", twoItemLedger(t))
	r, err := h.store.Get(h.ctx, ref.Kind, ref.Scope, ref.Name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.jobs.Enqueue(h.ctx, jobs.EnqueueParams{Queue: StepQueue, Kind: VerifyJobKind, Payload: []byte(r.ID)}); err != nil {
		t.Fatal(err)
	}

	if _, err := w.ProcessOnce(h.ctx); err != nil {
		t.Fatalf("ProcessOnce: %v", err)
	}
	if st := h.status(t, ref); st.Ledger[0].Proven {
		t.Fatal("an item was proven despite its verdict never reaching the record")
	}
}

// TestConsumptionSurvivesARestart: consumption is read back from the durable record, so a
// resumed run spends exactly what the record already spent rather than re-spending a
// verification a previous process consumed.
func TestConsumptionSurvivesARestart(t *testing.T) {
	ledger := twoItemLedger(t)
	recorded := []Verification{{Ref: "1", Item: ledger[0].ID, Passed: true, Provenance: ProvenanceExecuted}}

	var before Status
	before.SyncLedger(ledger)
	if n := before.ProveRecorded(newGate(t, RequireExecuted()), recorded, testNow); n != 1 {
		t.Fatalf("proved %d items, want 1", n)
	}
	enc, err := before.Encode()
	if err != nil {
		t.Fatal(err)
	}

	// A fresh process rehydrates the status and rebuilds the gate from nothing.
	var after Status
	if err := json.Unmarshal(enc, &after); err != nil {
		t.Fatal(err)
	}
	// The rehydrated gate holds no state of its own, so the only thing that can tell it
	// ref "1" is spent is the record: the proven item's own Evidence field.
	if err := after.Prove(newGate(t, RequireExecuted()), ledger[0].ID, recorded, testNow); !errors.Is(err, ErrEvidenceSpent) {
		t.Fatalf("a resumed run did not know its verification was already spent: %v", err)
	}
	if after.Ledger[0].Evidence != "1" {
		t.Fatalf("evidence = %q, want the original proof kept", after.Ledger[0].Evidence)
	}
	// The spent ref must not certify the second item on the resumed run.
	stolen := []Verification{{Ref: "1", Item: ledger[1].ID, Passed: true, Provenance: ProvenanceExecuted}}
	if err := after.Prove(newGate(t, RequireExecuted()), ledger[1].ID, stolen, testNow); !errors.Is(err, ErrEvidenceSpent) {
		t.Fatalf("a resumed run re-spent a consumed verification: %v", err)
	}
}

// TestTheProducerRunsBeforeTheRefusalDoes is the staging the rollout depends on: with the
// loop wired but the refusal not yet turned on, items still flip to proven from the record
// (so a build can be observed actually proving things) while a completion claim is still
// the model's to make. Without this split the refusal would have to be switched on ahead of
// any evidence that the producer works, which is what stalls every goal.
func TestTheProducerRunsBeforeTheRefusalDoes(t *testing.T) {
	ev := &fakeEvidence{}
	h := newHarness(t, alwaysMet{}, WithLedgerGate(ev, newGate(t, RequireExecuted())))
	ledger := twoItemLedger(t)
	ref := h.plannedGoal(t, "staged", ledger)
	if _, err := ev.Record(h.ctx, resource.Resource{}, ledger[0].ID, ItemVerdict{Passed: true, Executed: true}); err != nil {
		t.Fatal(err)
	}

	h.reconcile(t, ref)

	st := h.status(t, ref)
	if !st.Ledger[0].Proven {
		t.Fatal("the loop did not settle an item the record backs")
	}
	if st.Phase != PhaseConverged {
		t.Fatalf("phase = %q, want the claim still honoured until the refusal is turned on: %s", st.Phase, st.Message)
	}
}

// TestAProofSurvivesTheDispatchWrite: the ledger has two writers, so the dispatch write
// merges rather than adopting one side. Taking the worker's copy wholesale would drop the
// proof this very pass admitted, and the item would be re-verified forever.
func TestAProofSurvivesTheDispatchWrite(t *testing.T) {
	ledger := twoItemLedger(t)
	proven := []LedgerState{{ID: ledger[0].ID, Proven: true, Evidence: "1"}, {ID: ledger[1].ID}}
	fresh := []LedgerState{{ID: ledger[0].ID}, {ID: ledger[1].ID}}

	merged, added := mergeLedger(proven, fresh)
	if !added {
		t.Fatal("the merge did not report carrying a proof the record lacked")
	}
	if !merged[0].Proven || merged[0].Evidence != "1" {
		t.Fatalf("merged = %+v, want the proof and its evidence kept", merged)
	}

	// The shape comes from the freshest copy, so an item a concurrent planning step
	// appended in this window is not dropped by this write.
	grown := append(append([]LedgerState{}, fresh...), LedgerState{ID: "cccccccccccccccc"})
	merged, _ = mergeLedger(proven, grown)
	if len(merged) != 3 {
		t.Fatalf("merged %d items, want the freshest copy's three", len(merged))
	}
	// An earlier proof on the record is never restamped by a later merge.
	settled := []LedgerState{{ID: ledger[0].ID, Proven: true, Evidence: "first"}, {ID: ledger[1].ID}}
	merged, added = mergeLedger(proven, settled)
	if added || merged[0].Evidence != "first" {
		t.Fatalf("merged = %+v (added=%v), want the record's own earlier proof untouched", merged, added)
	}
}

// TestUnprovenReasonReportsAnAdmissibleItemAsTheAnomalyItIs: reaching the refusal with an
// item the gate would have admitted means the settling pass did not settle something it
// could have, which is a bug in the loop rather than an item nobody did the work for. It is
// reported as such instead of being papered over as "not done".
func TestUnprovenReasonReportsAnAdmissibleItemAsTheAnomalyItIs(t *testing.T) {
	if got := unprovenReason(nil, "x", nil); got != "admissible but not settled" {
		t.Fatalf("reason for a nil refusal = %q", got)
	}
}

// TestGateSelfTestCatchesARefusalOfExecutedEvidence: the provenance axis must never become a
// way to refuse the evidence the producer actually ran, so a gate that rejects an executed
// check is caught under either policy.
func TestGateSelfTestCatchesARefusalOfExecutedEvidence(t *testing.T) {
	refusesExecuted := func(itemID string, recorded []Verification, consumed map[string]bool) (string, error) {
		kept := make([]Verification, 0, len(recorded))
		for _, v := range recorded {
			if v.Provenance != ProvenanceExecuted {
				kept = append(kept, v)
			}
		}
		g := &EvidenceGate{}
		return g.admit(itemID, kept, consumed)
	}
	for _, requireExecuted := range []bool{false, true} {
		if err := selfTest(refusesExecuted, requireExecuted); !errors.Is(err, ErrGateBroken) {
			t.Fatalf("selfTest passed a gate that refuses executed evidence (requireExecuted=%v): %v", requireExecuted, err)
		}
	}
}

// TestABadStatusFailsTheVerifyStepTerminally: a status the worker cannot decode is not
// fixed by retrying, and a verify step that kept retrying it would burn the attempt budget
// before stalling the goal with the real cause.
func TestABadStatusFailsTheVerifyStepTerminally(t *testing.T) {
	h, w := producerHarness(t, &fakeVerifier{}, &fakeEvidence{})
	ref := h.plannedGoal(t, "corrupt", twoItemLedger(t))
	r, err := h.store.Get(h.ctx, ref.Kind, ref.Scope, ref.Name)
	if err != nil {
		t.Fatal(err)
	}
	r.Status = json.RawMessage(`"not a status object"`)
	if _, err := h.store.Put(h.ctx, r); err != nil {
		t.Fatal(err)
	}
	if _, err := h.jobs.Enqueue(h.ctx, jobs.EnqueueParams{Queue: StepQueue, Kind: VerifyJobKind, Payload: []byte(r.ID)}); err != nil {
		t.Fatal(err)
	}

	if _, err := w.ProcessOnce(h.ctx); err != nil {
		t.Fatalf("ProcessOnce: %v", err)
	}
	claimed, err := h.jobs.Claim(h.ctx, jobs.ClaimParams{Queue: StepQueue, Limit: 5, LeaseFor: int64(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 0 {
		t.Fatal("an undecodable status was left retryable")
	}
}

// TestAVerificationFailureIsRetriedNotSwallowed: a verifier that fails outright (as opposed
// to reporting it could not run the check) is the run's own machinery breaking, so the job
// fails and rides the retry ladder rather than silently recording nothing.
func TestAVerificationFailureIsRetriedNotSwallowed(t *testing.T) {
	ev := &fakeEvidence{}
	ver := &fakeVerifier{err: fault.New(fault.Transient, "verifier_down", "the verifier is briefly unavailable")}
	h, w := producerHarness(t, ver, ev)
	ref := h.plannedGoal(t, "flaky", twoItemLedger(t))
	r, err := h.store.Get(h.ctx, ref.Kind, ref.Scope, ref.Name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.jobs.Enqueue(h.ctx, jobs.EnqueueParams{Queue: StepQueue, Kind: VerifyJobKind, Payload: []byte(r.ID)}); err != nil {
		t.Fatal(err)
	}

	if _, err := w.ProcessOnce(h.ctx); err != nil {
		t.Fatalf("ProcessOnce: %v", err)
	}
	if len(ev.recs) != 0 {
		t.Fatalf("recorded %+v for a verification that never produced a verdict", ev.recs)
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

// TestAnUnrunnableCheckIsNotReportedAsUnchecked: both reach the gate as ErrNoEvidence,
// because an unrunnable check records a verdict that did not pass exactly as a failing one
// does. They need opposite responses, though: one is work still to do, the other is a check
// the host or the clause cannot execute, and no amount of further building fixes it.
func TestAnUnrunnableCheckIsNotReportedAsUnchecked(t *testing.T) {
	ledger, err := AppendItems(nil,
		LedgerItem{Item: "nobody checked this", Verify: "true"},
		LedgerItem{Item: "its check ran and failed", Verify: "false"},
		LedgerItem{Item: "its check could not run here", Verify: "a command this host cannot run"},
	)
	if err != nil {
		t.Fatal(err)
	}
	var st Status
	st.SyncLedger(ledger)
	recorded := []Verification{
		{Ref: "1", Item: ledger[1].ID, Passed: false, Provenance: ProvenanceExecuted},
		{Ref: "2", Item: ledger[2].ID, Passed: false, Provenance: ProvenanceAsserted},
	}

	reasons := st.UnprovenReasons(newGate(t, RequireExecuted()), recorded)
	want := []string{
		"no recorded passing verification",
		"its check ran and did not pass",
		"its check could not be run",
	}
	for i, w := range want {
		if !strings.Contains(reasons[i], w) {
			t.Fatalf("reason %d = %q, want it to say %q", i, reasons[i], w)
		}
	}
}

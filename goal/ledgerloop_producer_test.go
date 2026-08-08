package goal

// The verify step: the job that runs a check and writes what it found. It records the
// verdict and proves nothing itself, because proving is the settling pass's decision
// from the record. Its failures divide cleanly: a status it cannot decode is terminal,
// while a verifier that breaks is the run's own machinery and rides the retry ladder.

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/jobs"
	"github.com/ionalpha/flynn/resource"
)

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

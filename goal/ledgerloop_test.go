package goal

// Shared harness for the evidence-ledger tests: a recorder and a verifier a test can
// script, the two convergence stubs, and the helpers that plan a goal, edit its status
// and complete one job. The tests themselves sit in the ledgerloop_*_test.go files
// alongside this one.

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/ionalpha/flynn/jobs"
	"github.com/ionalpha/flynn/reconcile"
	"github.com/ionalpha/flynn/resource"
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

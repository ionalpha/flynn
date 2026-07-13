package goal

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/jobs"
	"github.com/ionalpha/flynn/reconcile"
	"github.com/ionalpha/flynn/resource"
)

// errStoreDown is the sentinel the injected store failures carry, so an assertion
// names the exact cause rather than matching on a message.
var errStoreDown = errors.New("goal test: store unavailable")

// faultyStore injects deterministic failures into a resource.Store, the one port
// the reconciler and worker write every durable decision through. There is no
// testkit injector for a resource.Store (the shared Faulty* set covers the log,
// the queue, the executor, and the bus), so the failures are driven here by
// per-method call counters: a hook is handed the 1-based call index and returns
// the error to inject, or nil to pass through. mutateGet rewrites a record on the
// way out, which is how a spec or status that cannot be decoded is delivered past
// the store's own admission checks. Every test drives it from one goroutine.
type faultyStore struct {
	resource.Store

	getCalls, getByIDCalls, putCalls, deleteCalls int

	onGet     func(call int) error
	mutateGet func(call int, r *resource.Resource)
	onGetByID func(call int) error
	onPut     func(call int, r resource.Resource) error
	onDelete  func(call int) error
}

func (s *faultyStore) Get(ctx context.Context, kind string, scope resource.Scope, name string) (resource.Resource, error) {
	s.getCalls++
	if s.onGet != nil {
		if err := s.onGet(s.getCalls); err != nil {
			return resource.Resource{}, err
		}
	}
	r, err := s.Store.Get(ctx, kind, scope, name)
	if err == nil && s.mutateGet != nil {
		s.mutateGet(s.getCalls, &r)
	}
	return r, err
}

func (s *faultyStore) GetByID(ctx context.Context, id string) (resource.Resource, error) {
	s.getByIDCalls++
	if s.onGetByID != nil {
		if err := s.onGetByID(s.getByIDCalls); err != nil {
			return resource.Resource{}, err
		}
	}
	return s.Store.GetByID(ctx, id)
}

func (s *faultyStore) Put(ctx context.Context, r resource.Resource) (resource.Resource, error) {
	s.putCalls++
	if s.onPut != nil {
		if err := s.onPut(s.putCalls, r); err != nil {
			return resource.Resource{}, err
		}
	}
	return s.Store.Put(ctx, r)
}

func (s *faultyStore) Delete(ctx context.Context, kind string, scope resource.Scope, name string) error {
	s.deleteCalls++
	if s.onDelete != nil {
		if err := s.onDelete(s.deleteCalls); err != nil {
			return err
		}
	}
	return s.Store.Delete(ctx, kind, scope, name)
}

// faultHarness builds a reconciler over a fault-injectable store. The returned
// store's hooks are nil, so a test arms them only after its fixture is in place
// and the setup writes do not trip them.
func faultHarness(t *testing.T, q jobs.Queue, stop StopEvaluator, opts ...Option) (*harness, *faultyStore) {
	t.Helper()
	m := clock.NewManual(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	reg := resource.NewRegistry()
	if err := resource.RegisterCoreKinds(reg); err != nil {
		t.Fatal(err)
	}
	if err := RegisterKind(reg); err != nil {
		t.Fatal(err)
	}
	inner := resource.NewMemory(reg, resource.WithClock(m))
	fs := &faultyStore{Store: inner}
	h := &harness{ctx: context.Background(), store: fs, clk: m}
	if q == nil {
		mq := jobs.NewMemory(jobs.WithClock(m))
		h.jobs, q = mq, mq
	}
	h.gr = NewReconciler(fs, q, m, stop, opts...)
	return h, fs
}

// rawStatus reads a goal's status straight from the inner store, bypassing the
// fault hooks so an assertion always sees the truth.
func rawStatus(t *testing.T, fs *faultyStore, ref reconcile.Ref) Status {
	t.Helper()
	r, err := fs.Store.Get(context.Background(), ref.Kind, ref.Scope, ref.Name)
	if err != nil {
		t.Fatalf("get goal: %v", err)
	}
	st, err := DecodeStatus(r)
	if err != nil {
		t.Fatalf("decode status: %v", err)
	}
	return st
}

// putRaw writes a goal record directly to the inner store, which is how a status
// the reconciler cannot decode gets stored: the store validates the spec against
// the kind's schema but never the status.
func putRaw(t *testing.T, fs *faultyStore, r resource.Resource) resource.Resource {
	t.Helper()
	out, err := fs.Store.Put(context.Background(), r)
	if err != nil {
		t.Fatalf("put goal: %v", err)
	}
	return out
}

func goalSpec(t *testing.T, s Spec) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestWithPollIntervalSetsInFlightPoll: the option decides how soon a dispatched
// step is re-checked when its completion signal never arrives, and a
// non-positive value leaves the default in place rather than disabling the poll.
func TestWithPollIntervalSetsInFlightPoll(t *testing.T) {
	for _, tc := range []struct {
		name string
		poll time.Duration
		want time.Duration
	}{
		{name: "override", poll: 2 * time.Second, want: 2 * time.Second},
		{name: "zero keeps the default", poll: 0, want: DefaultPollInterval},
		{name: "negative keeps the default", poll: -time.Second, want: DefaultPollInterval},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, stopAfter{at: 99}, WithPollInterval(tc.poll))
			ref := h.createGoal(t, "g", Spec{Objective: "o", StopCondition: "c"})
			res := h.reconcile(t, ref) // finalizer + dispatch in one pass
			if res.RequeueAfter != tc.want {
				t.Fatalf("RequeueAfter = %v, want %v", res.RequeueAfter, tc.want)
			}
		})
	}
}

// TestReconcileStoreReadFailurePropagates: a store that cannot be read fails the
// reconcile outright. The stall path cannot record the cause either (it reads the
// same store), so the read error is what surfaces, and the controller retries.
func TestReconcileStoreReadFailurePropagates(t *testing.T) {
	h, fs := faultHarness(t, nil, stopAfter{at: 99})
	ref := h.createGoal(t, "g", Spec{Objective: "o", StopCondition: "c"})
	fs.onGet = func(int) error { return errStoreDown }

	_, err := h.gr.Reconcile(h.ctx, ref)
	if !errors.Is(err, errStoreDown) {
		t.Fatalf("Reconcile error = %v, want the store failure", err)
	}
}

// TestReconcileUndecodableRecordStalls: a goal whose spec or status cannot be
// decoded can never be reconciled, so it is settled as stalled with the cause
// instead of being left waiting on a step that will never be dispatched. The
// stall write itself must survive a status it could not decode.
func TestReconcileUndecodableRecordStalls(t *testing.T) {
	for _, tc := range []struct {
		name    string
		arrange func(t *testing.T, h *harness, fs *faultyStore) reconcile.Ref
		wantMsg string
	}{
		{
			name: "spec",
			arrange: func(t *testing.T, h *harness, fs *faultyStore) reconcile.Ref {
				ref := h.createGoal(t, "g", Spec{Objective: "o", StopCondition: "c"})
				// The store validates the spec on write, so the undecodable spec is
				// delivered on the read instead (objective is typed as a string), and
				// only on the reconcile's own read: the stall then reads the stored
				// record back and its write is admitted normally.
				fs.mutateGet = func(call int, r *resource.Resource) {
					if call == 1 {
						r.Spec = json.RawMessage(`{"objective":123}`)
					}
				}
				return ref
			},
			wantMsg: "goal_spec_decode",
		},
		{
			name: "status",
			arrange: func(t *testing.T, _ *harness, fs *faultyStore) reconcile.Ref {
				putRaw(t, fs, resource.Resource{
					APIVersion: GroupVersion, Kind: Kind, Name: "g",
					Spec:   goalSpec(t, Spec{Objective: "o", StopCondition: "c"}),
					Status: json.RawMessage(`"not a status object"`),
				})
				return reconcile.Ref{Kind: Kind, Name: "g"}
			},
			wantMsg: "goal_status_decode",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, fs := faultHarness(t, nil, stopAfter{at: 99})
			ref := tc.arrange(t, h, fs)

			if _, err := h.gr.Reconcile(h.ctx, ref); err != nil {
				t.Fatalf("a terminal decode failure must settle the goal, not error: %v", err)
			}
			fs.mutateGet = nil
			st := rawStatus(t, fs, ref)
			if st.Phase != PhaseStalled || !hasCond(st, CondStalled, "True") {
				t.Fatalf("undecodable goal was not stalled: %+v", st)
			}
			if !strings.Contains(st.Message, tc.wantMsg) {
				t.Fatalf("stall message = %q, want it to name %q", st.Message, tc.wantMsg)
			}
			if st.InFlight != nil {
				t.Fatalf("a stalled goal holds an in-flight step: %+v", st.InFlight)
			}
		})
	}
}

// TestReconcileWriteConflictIsTransient: losing an optimistic-concurrency race is
// not a failure of the goal. It must be classified Transient so the controller
// backs off and retries against a fresh read, rather than reading the lost race as
// terminal and stalling a perfectly healthy goal.
func TestReconcileWriteConflictIsTransient(t *testing.T) {
	h, fs := faultHarness(t, nil, stopAfter{at: 99})
	ref := h.createGoal(t, "g", Spec{Objective: "o", StopCondition: "c"})
	fs.onPut = func(int, resource.Resource) error { return resource.ErrConflict }

	_, err := h.gr.Reconcile(h.ctx, ref)
	if !errors.Is(err, resource.ErrConflict) {
		t.Fatalf("Reconcile error = %v, want the write conflict", err)
	}
	if got := fault.Classify(err); got != fault.Transient {
		t.Fatalf("write conflict classified %q, want %q (a lost race must be retried)", got, fault.Transient)
	}
	fs.onPut = nil
	if st := rawStatus(t, fs, ref); st.Phase == PhaseStalled {
		t.Fatal("a lost write race stalled the goal instead of retrying")
	}
}

// TestConvergenceIsNeverReportedWithoutItsWrite: the write that records a settled
// goal is the one write that may not be silently dropped. When it fails, the goal
// must not read as converged; the failure is carried into the goal's own settled
// state, so a caller sees a stall with the cause instead of a convergence that was
// never persisted.
func TestConvergenceIsNeverReportedWithoutItsWrite(t *testing.T) {
	h, fs := faultHarness(t, nil, stopAfter{at: 0}) // the stop condition holds at once
	ref := h.createGoal(t, "g", Spec{Objective: "o", StopCondition: "c"})

	// Fail only the write that carries the converged status, so the finalizer write
	// still lands and the reconcile reaches the settle.
	fs.onPut = func(_ int, r resource.Resource) error {
		st, err := DecodeStatus(r)
		if err == nil && st.Phase == PhaseConverged {
			return errStoreDown
		}
		return nil
	}
	if _, err := h.gr.Reconcile(h.ctx, ref); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	fs.onPut = nil

	st := rawStatus(t, fs, ref)
	if st.Phase == PhaseConverged {
		t.Fatal("convergence was reported without its write landing")
	}
	if st.Phase != PhaseStalled || !strings.Contains(st.Message, errStoreDown.Error()) {
		t.Fatalf("a failed settle write left the goal %q (%q), want Stalled carrying the cause", st.Phase, st.Message)
	}
}

// TestPersistStatusRejectsUnencodableStatus: a status that cannot be encoded is a
// terminal fault, not a retryable one, so it settles the goal rather than looping
// the controller on a write that can never succeed. Only a corrupt checkpoint (the
// executor-owned opaque field) can reach this, so it is driven directly.
func TestPersistStatusRejectsUnencodableStatus(t *testing.T) {
	h, _ := faultHarness(t, nil, stopAfter{at: 99})
	ref := h.createGoal(t, "g", Spec{Objective: "o", StopCondition: "c"})
	r, err := h.store.Get(h.ctx, ref.Kind, ref.Scope, ref.Name)
	if err != nil {
		t.Fatal(err)
	}

	bad := Status{Phase: PhaseRunning, Checkpoint: json.RawMessage(`{"unterminated":`)}
	err = h.gr.persistStatus(h.ctx, r, bad, r.SpecHash)
	if err == nil {
		t.Fatal("an unencodable status was persisted")
	}
	if got := fault.Classify(err); got != fault.Terminal {
		t.Fatalf("encode failure classified %q, want %q", got, fault.Terminal)
	}
	if !strings.Contains(err.Error(), "goal_status_encode") {
		t.Fatalf("error = %v, want it to name the encode fault", err)
	}
}

// TestStallSkipsGoalThatVanished: a goal deleted between the failing reconcile and
// the stall write has nothing left to record. The stall is a no-op and the
// reconcile reports success, so the controller does not spin on a resource that is
// already gone.
func TestStallSkipsGoalThatVanished(t *testing.T) {
	h, fs := faultHarness(t, nil, failingStop{err: errors.New("model rejected the request")})
	ref := h.createGoal(t, "g", Spec{Objective: "o", StopCondition: "c"})

	// The reconcile reads the goal once (call 1) and fails terminally at the stop
	// evaluator. The goal is deleted in that window, so the stall's read back
	// (call 2) misses.
	fs.onGet = func(call int) error {
		if call >= 2 {
			return resource.ErrNotFound
		}
		return nil
	}

	res, err := h.gr.Reconcile(h.ctx, ref)
	if err != nil {
		t.Fatalf("a stall over a vanished goal must be a no-op, got %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("a vanished goal was requeued after %v, want no requeue", res.RequeueAfter)
	}
	fs.onGet = nil
	if st := rawStatus(t, fs, ref); st.Phase == PhaseStalled {
		t.Fatal("the stall wrote to a goal it had just read as gone")
	}
}

// TestWakeOwnerToleratesUnreadableOwner: waking a parked owner is best effort. A
// child that settles while its owner cannot be read must still settle: a lost wake
// costs latency (the recheck fallback covers it), never correctness.
func TestWakeOwnerToleratesUnreadableOwner(t *testing.T) {
	wake := &recordingBus{}
	h, fs := faultHarness(t, nil, stopAfter{at: 99}, WithWakeBus(wake))

	// A child owned by an id that resolves to nothing, and a status the reconciler
	// cannot decode, so the reconcile settles as stalled (via the stall path, which
	// does not garbage-collect the orphan) and reaches the owner wake.
	child := resource.Resource{
		APIVersion: GroupVersion, Kind: Kind, Name: "child",
		Spec:   goalSpec(t, Spec{Objective: "o", StopCondition: "c"}),
		Status: json.RawMessage(`"not a status object"`),
	}
	child.OwnerReferences = []resource.OwnerReference{{
		APIVersion: GroupVersion, Kind: Kind, Name: "ghost", ID: "no-such-owner", Controller: true,
	}}
	putRaw(t, fs, child)
	ref := reconcile.Ref{Kind: Kind, Name: "child"}

	if _, err := h.gr.Reconcile(h.ctx, ref); err != nil {
		t.Fatalf("an unreadable owner must not fail the child's reconcile: %v", err)
	}
	if st := rawStatus(t, fs, ref); st.Phase != PhaseStalled {
		t.Fatalf("child phase = %q, want Stalled", st.Phase)
	}
	// There is no owner to wake: the wake is abandoned rather than retried or
	// signalled into the void, and the child settles regardless.
	if got := wake.published(StepSubject); len(got) != 0 {
		t.Fatalf("published %d wake signals for an owner that does not exist, want 0", len(got))
	}
}

// TestOrphanDeleteConflictIsTransient: an orphan whose self-delete loses a write
// race is retried, not abandoned. Abandoning it would leak the subtree the deleted
// owner created.
func TestOrphanDeleteConflictIsTransient(t *testing.T) {
	h, fs := faultHarness(t, nil, stopAfter{at: 99})
	parent := h.createGoal(t, "parent", Spec{Objective: "o", StopCondition: "c"})
	pr, err := h.store.Get(h.ctx, parent.Kind, parent.Scope, parent.Name)
	if err != nil {
		t.Fatal(err)
	}
	child := resource.Resource{
		APIVersion: GroupVersion, Kind: Kind, Name: "child",
		Spec: goalSpec(t, Spec{Objective: "child", StopCondition: "c"}),
	}
	child.OwnerReferences = []resource.OwnerReference{{
		APIVersion: GroupVersion, Kind: Kind, Name: pr.Name, ID: pr.ID, Controller: true,
	}}
	putRaw(t, fs, child)
	if err := h.store.Delete(h.ctx, parent.Kind, parent.Scope, parent.Name); err != nil {
		t.Fatal(err)
	}

	fs.onDelete = func(int) error { return resource.ErrConflict }
	_, err = h.gr.Reconcile(h.ctx, reconcile.Ref{Kind: Kind, Name: "child"})
	if !errors.Is(err, resource.ErrConflict) {
		t.Fatalf("orphan reap error = %v, want the write conflict", err)
	}
	if got := fault.Classify(err); got != fault.Transient {
		t.Fatalf("orphan reap conflict classified %q, want %q", got, fault.Transient)
	}
	if _, err := fs.Store.Get(h.ctx, Kind, resource.Scope{}, "child"); err != nil {
		t.Fatalf("the orphan was dropped after a failed reap: %v", err)
	}
}

// TestFinalizeLeavesForeignFinalizers: the reconciler removes only its own
// finalizer. A goal another controller still holds stays terminating, so that
// controller's cleanup is never skipped, and a goal our finalizer is not on at all
// is left entirely alone.
func TestFinalizeLeavesForeignFinalizers(t *testing.T) {
	const foreign = "other.example/keep"

	for _, tc := range []struct {
		name       string
		finalizers []string
		wantAfter  []string
	}{
		{name: "ours alongside a foreign one", finalizers: []string{foreign, Finalizer}, wantAfter: []string{foreign}},
		{name: "foreign only", finalizers: []string{foreign}, wantAfter: []string{foreign}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cleaner := &recordingCleaner{}
			h, fs := faultHarness(t, nil, stopAfter{at: 99}, WithCleaner(cleaner))
			putRaw(t, fs, resource.Resource{
				APIVersion: GroupVersion, Kind: Kind, Name: "g",
				Spec:     goalSpec(t, Spec{Objective: "o", StopCondition: "c"}),
				Envelope: resource.Envelope{Finalizers: tc.finalizers},
			})
			ref := reconcile.Ref{Kind: Kind, Name: "g"}
			if err := h.store.Delete(h.ctx, ref.Kind, ref.Scope, ref.Name); err != nil {
				t.Fatal(err)
			}

			if _, err := h.gr.Reconcile(h.ctx, ref); err != nil {
				t.Fatalf("finalize: %v", err)
			}
			r, err := fs.Store.Get(h.ctx, ref.Kind, ref.Scope, ref.Name)
			if err != nil {
				t.Fatalf("the goal was deleted while a foreign finalizer still held it: %v", err)
			}
			if len(r.Finalizers) != len(tc.wantAfter) || r.Finalizers[0] != tc.wantAfter[0] {
				t.Fatalf("finalizers = %v, want %v", r.Finalizers, tc.wantAfter)
			}
			wantCleanups := 1
			if !hasFinalizer(tc.finalizers, Finalizer) {
				wantCleanups = 0 // our finalizer is not on the goal: there is nothing of ours to tear down
			}
			if cleaner.calls != wantCleanups {
				t.Fatalf("cleanup ran %d times, want %d", cleaner.calls, wantCleanups)
			}
		})
	}
}

// TestFinalizeWriteFailureKeepsFinalizer: if the write that drops our finalizer
// fails, the goal stays terminating and the error surfaces, so the delete is
// retried. Reporting success here would strand the goal terminating forever with
// no controller left to finish it.
func TestFinalizeWriteFailureKeepsFinalizer(t *testing.T) {
	h, fs := faultHarness(t, nil, stopAfter{at: 99})
	ref := h.createGoal(t, "g", Spec{Objective: "o", StopCondition: "c"})
	h.reconcile(t, ref) // adds our finalizer
	if err := h.store.Delete(h.ctx, ref.Kind, ref.Scope, ref.Name); err != nil {
		t.Fatal(err)
	}

	fs.onPut = func(int, resource.Resource) error { return errStoreDown }
	if _, err := h.gr.Reconcile(h.ctx, ref); !errors.Is(err, errStoreDown) {
		t.Fatalf("finalize error = %v, want the failed write", err)
	}
	fs.onPut = nil
	r, err := fs.Store.Get(h.ctx, ref.Kind, ref.Scope, ref.Name)
	if err != nil {
		t.Fatalf("the goal was deleted despite the failed finalizer write: %v", err)
	}
	if !hasFinalizer(r.Finalizers, Finalizer) {
		t.Fatal("the finalizer was dropped even though its write failed")
	}
}

// TestParkedGoalRechecksOnTheDerivedFallback: with no explicit recheck override, a
// parked goal's fallback re-check is derived from the poll interval, so a lost wake
// costs a bounded, poll-proportional delay and not an unbounded wait.
func TestParkedGoalRechecksOnTheDerivedFallback(t *testing.T) {
	const poll = time.Second
	h := newHarness(t, stopAfter{at: 99}, WithPollInterval(poll))
	w := NewWorker(h.store, h.jobs, h.clk, waitingExec{})
	ref := h.createGoal(t, "g", Spec{Objective: "o", StopCondition: "c"})

	h.reconcile(t, ref) // finalizer + dispatch
	if ok, err := w.ProcessOnce(h.ctx); err != nil || !ok {
		t.Fatalf("the step did not run (ok=%v err=%v)", ok, err)
	}
	res := h.reconcile(t, ref) // observes the park

	if st := h.status(t, ref); st.WaitingSince == nil {
		t.Fatal("the goal was not parked by its waiting step")
	}
	want := DefaultWaitRecheckFactor * poll
	if res.RequeueAfter != want {
		t.Fatalf("parked requeue = %v, want %v (the fallback derives from the poll)", res.RequeueAfter, want)
	}
}

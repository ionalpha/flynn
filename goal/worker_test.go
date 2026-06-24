package goal

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/jobs"
	"github.com/ionalpha/flynn/reconcile"
	"github.com/ionalpha/flynn/resource"
)

// fakeExec records each call and the checkpoint it was handed, and optionally
// emits a checkpoint to persist.
type fakeExec struct {
	calls int
	saw   []string
	emit  string
}

func (f *fakeExec) Execute(_ context.Context, r resource.Resource) (json.RawMessage, error) {
	f.calls++
	st, _ := DecodeStatus(r)
	f.saw = append(f.saw, string(st.Checkpoint))
	if f.emit != "" {
		return json.RawMessage(`"` + f.emit + `"`), nil
	}
	return nil, nil
}

// failCompleteOnce wraps a queue and fails the first Complete to simulate a worker
// crashing after the work is done but before the job is marked complete.
type failCompleteOnce struct {
	*jobs.MemoryQueue
	failed bool
}

func (q *failCompleteOnce) Complete(ctx context.Context, id string) error {
	if !q.failed {
		q.failed = true
		return errors.New("simulated crash before complete")
	}
	return q.MemoryQueue.Complete(ctx, id)
}

func workerHarness(t *testing.T, q jobs.Queue, stop StopEvaluator, exec StepExecutor, lease time.Duration) (*harness, *Worker) {
	t.Helper()
	m := clock.NewManual(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	reg := resource.NewRegistry()
	if err := resource.RegisterCoreKinds(reg); err != nil {
		t.Fatal(err)
	}
	if err := RegisterKind(reg); err != nil {
		t.Fatal(err)
	}
	store := resource.NewMemory(reg, resource.WithClock(m))
	gr := NewReconciler(store, q, m, stop)
	w := NewWorker(store, q, exec, WithLease(lease))
	h := &harness{ctx: context.Background(), store: store, jobs: nil, gr: gr, clk: m}
	return h, w
}

// TestWorkerDrivesConvergence runs the reconciler and worker together: the
// reconciler dispatches steps, the worker executes them, and the goal converges.
func TestWorkerDrivesConvergence(t *testing.T) {
	m := clock.NewManual(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	q := jobs.NewMemory(jobs.WithClock(m))
	exec := &fakeExec{}
	h, w := workerHarness(t, q, stopAfter{at: 2}, exec, time.Minute)
	h.clk = m
	ref := h.createGoal(t, "g", Spec{Objective: "o", StopCondition: "c"})

	for i := 0; i < 20; i++ {
		if _, err := h.gr.Reconcile(h.ctx, ref); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if _, err := w.ProcessOnce(h.ctx); err != nil {
			t.Fatalf("worker: %v", err)
		}
		if h.status(t, ref).Phase == PhaseConverged {
			break
		}
	}

	st := h.status(t, ref)
	if st.Phase != PhaseConverged {
		t.Fatalf("goal did not converge: %+v", st)
	}
	if exec.calls != 2 {
		t.Fatalf("worker executed %d steps, want 2", exec.calls)
	}
}

// TestWorkerCrashResumesFromCheckpoint is the durability property: a step that
// persists a checkpoint then crashes before completing is re-leased and re-run,
// and the re-run sees the persisted checkpoint instead of restarting blank.
func TestWorkerCrashResumesFromCheckpoint(t *testing.T) {
	m := clock.NewManual(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	q := &failCompleteOnce{MemoryQueue: jobs.NewMemory(jobs.WithClock(m))}
	exec := &fakeExec{emit: "ckpt-1"}
	store, gr, w, ref := func() (resource.Store, *GoalReconciler, *Worker, reconcile.Ref) {
		reg := resource.NewRegistry()
		_ = resource.RegisterCoreKinds(reg)
		_ = RegisterKind(reg)
		s := resource.NewMemory(reg, resource.WithClock(m))
		raw, _ := json.Marshal(Spec{Objective: "o", StopCondition: "c"})
		r, err := s.Put(context.Background(), resource.Resource{APIVersion: GroupVersion, Kind: Kind, Name: "g", Spec: raw})
		if err != nil {
			t.Fatal(err)
		}
		return s, NewReconciler(s, q, m, stopAfter{at: 99}), NewWorker(s, q, exec, WithLease(time.Minute)), reconcile.Ref{Kind: Kind, Name: r.Name}
	}()
	ctx := context.Background()

	mustReconcile(t, gr, ctx, ref) // add finalizer
	mustReconcile(t, gr, ctx, ref) // dispatch step

	// First run: executes, persists ckpt-1, then "crashes" on Complete.
	if _, err := w.ProcessOnce(ctx); err == nil {
		t.Fatal("expected the simulated crash on Complete")
	}
	cur, _ := store.Get(ctx, ref.Kind, ref.Scope, ref.Name)
	if st, _ := DecodeStatus(cur); string(st.Checkpoint) != `"ckpt-1"` {
		t.Fatalf("checkpoint not persisted before crash: %q", st.Checkpoint)
	}

	// The crashed worker's lease lapses; the step is re-leased and re-run.
	m.Advance(2 * time.Minute)
	processed, err := w.ProcessOnce(ctx)
	if err != nil || !processed {
		t.Fatalf("re-lease did not re-run the step: processed=%v err=%v", processed, err)
	}
	if exec.calls != 2 {
		t.Fatalf("executor ran %d times, want 2 (crash + resume)", exec.calls)
	}
	if exec.saw[1] != `"ckpt-1"` {
		t.Fatalf("resumed step did not see the checkpoint: saw %q", exec.saw[1])
	}
}

// TestWorkerSkipsTerminatingGoal completes a step without executing when the goal
// is being deleted, so a pending step never keeps a deleting goal alive.
func TestWorkerSkipsTerminatingGoal(t *testing.T) {
	m := clock.NewManual(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	q := jobs.NewMemory(jobs.WithClock(m))
	exec := &fakeExec{}
	h, w := workerHarness(t, q, stopAfter{at: 99}, exec, time.Minute)
	h.clk = m
	ref := h.createGoal(t, "g", Spec{Objective: "o", StopCondition: "c"})

	h.mustReconcile(t, ref) // finalizer
	h.mustReconcile(t, ref) // dispatch a step (job now queued)

	// Request deletion: the goal is terminating (finalizer holds it live).
	if err := h.store.Delete(h.ctx, ref.Kind, ref.Scope, ref.Name); err != nil {
		t.Fatal(err)
	}
	processed, err := w.ProcessOnce(h.ctx)
	if err != nil || !processed {
		t.Fatalf("worker should claim and complete the step: processed=%v err=%v", processed, err)
	}
	if exec.calls != 0 {
		t.Fatalf("worker executed a step for a terminating goal: %d", exec.calls)
	}
}

func mustReconcile(t *testing.T, gr *GoalReconciler, ctx context.Context, ref reconcile.Ref) {
	t.Helper()
	if _, err := gr.Reconcile(ctx, ref); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

func (h *harness) mustReconcile(t *testing.T, ref reconcile.Ref) {
	t.Helper()
	mustReconcile(t, h.gr, h.ctx, ref)
}

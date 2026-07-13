package goal

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/jobs"
	"github.com/ionalpha/flynn/resource"
)

// enqueueStep queues a step job for the given goal id directly, without a
// reconcile pass. A goal whose status the reconciler cannot decode never
// dispatches a step of its own, so the worker's handling of that record is only
// reachable this way.
func enqueueStep(t *testing.T, q jobs.Queue, goalID string) {
	t.Helper()
	if _, err := q.Enqueue(context.Background(), jobs.EnqueueParams{
		Queue: StepQueue, Kind: StepJobKind, Payload: []byte(goalID),
	}); err != nil {
		t.Fatalf("enqueue step: %v", err)
	}
}

// TestWorkerStepLoadFailureFailsTheJob: a step whose goal cannot be read from the
// store is failed, not silently completed. The cause is unclassified, so it is not
// retryable and the job goes dead at once, which surfaces the fault as a stalled
// goal instead of an unbounded retry loop against a store that is down.
func TestWorkerStepLoadFailureFailsTheJob(t *testing.T) {
	m := clock.NewManual(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	q := jobs.NewMemory(jobs.WithClock(m))
	h, fs := faultHarness(t, q, stopAfter{at: 99})
	ref := h.createGoal(t, "g", Spec{Objective: "o", StopCondition: "c"})
	h.reconcile(t, ref) // finalizer + dispatch

	exec := &fakeExec{}
	w := NewWorker(fs, q, m, exec, WithLease(time.Minute))
	fs.onGetByID = func(int) error { return errStoreDown }

	processed, err := w.ProcessOnce(h.ctx)
	if err != nil || !processed {
		t.Fatalf("the step should be claimed and failed, not error out: processed=%v err=%v", processed, err)
	}
	if exec.calls != 0 {
		t.Fatalf("the executor ran %d times for a goal that could not be loaded", exec.calls)
	}
	fs.onGetByID = nil

	// The step is dead: no backoff window returns it to the queue.
	m.Advance(time.Hour)
	if processed, _ := w.ProcessOnce(h.ctx); processed {
		t.Fatal("a step whose goal could not be read was retried; that cause is not retryable")
	}
	st := rawStatus(t, fs, ref)
	if st.InFlight == nil {
		t.Fatal("the goal lost its in-flight reservation")
	}
	job, err := q.Get(h.ctx, st.InFlight.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != jobs.StateDead {
		t.Fatalf("job state = %q, want %q", job.State, jobs.StateDead)
	}
	if !strings.Contains(job.LastError, errStoreDown.Error()) {
		t.Fatalf("job error = %q, want the store failure", job.LastError)
	}
}

// TestWorkerProgressWritesAreBestEffortOnACorruptStatus: the two status writes a
// worker makes (the checkpoint and the wait mark) are best effort. A goal whose
// status cannot be decoded must not fail the step: the job still completes, so a
// corrupt record cannot wedge the queue behind a step that can never be finished.
// The reconciler is what settles such a goal, not the worker.
func TestWorkerProgressWritesAreBestEffortOnACorruptStatus(t *testing.T) {
	const corrupt = `"not a status object"`

	for _, tc := range []struct {
		name string
		exec StepExecutor
	}{
		{name: "checkpoint", exec: &fakeExec{emit: "ckpt-1"}},
		{name: "wait mark", exec: waitingExec{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := clock.NewManual(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
			q := jobs.NewMemory(jobs.WithClock(m))
			h, fs := faultHarness(t, q, stopAfter{at: 99})
			created := putRaw(t, fs, resource.Resource{
				APIVersion: GroupVersion, Kind: Kind, Name: "g",
				Spec:   goalSpec(t, Spec{Objective: "o", StopCondition: "c"}),
				Status: json.RawMessage(corrupt),
			})
			enqueueStep(t, q, created.ID)

			w := NewWorker(fs, q, m, tc.exec, WithLease(time.Minute))
			processed, err := w.ProcessOnce(h.ctx)
			if err != nil || !processed {
				t.Fatalf("a corrupt status must not fail the step: processed=%v err=%v", processed, err)
			}

			r, err := fs.Store.GetByID(h.ctx, created.ID)
			if err != nil {
				t.Fatal(err)
			}
			if string(r.Status) != corrupt {
				t.Fatalf("status = %s, want the undecodable record left as it was", r.Status)
			}
			left, err := q.Claim(h.ctx, jobs.ClaimParams{Queue: StepQueue, Limit: 10, LeaseFor: int64(time.Minute)})
			if err != nil {
				t.Fatal(err)
			}
			if len(left) != 0 {
				t.Fatalf("%d step jobs left claimable; the step was not completed", len(left))
			}
		})
	}
}

// TestRunReturnsWhenContextIsAlreadyCancelled: a worker started on a cancelled
// context does no work and returns at once, so a shutdown that races a worker
// launch cannot claim a step nobody will finish.
func TestRunReturnsWhenContextIsAlreadyCancelled(t *testing.T) {
	m := clock.NewManual(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	q := jobs.NewMemory(jobs.WithClock(m))
	exec := &fakeExec{}
	w := NewWorker(resource.NewMemory(resource.NewRegistry()), q, m, exec)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(ctx, time.Hour)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return on an already-cancelled context")
	}
	if exec.calls != 0 {
		t.Fatalf("a cancelled worker executed %d steps", exec.calls)
	}
}

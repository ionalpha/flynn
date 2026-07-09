package goal

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/ionalpha/flynn/reconcile"
	"github.com/ionalpha/flynn/resource"
)

// waitingExec parks the step on external state, the one non-terminal outcome that
// still ends the worker's turn on the job.
type waitingExec struct{}

func (waitingExec) Execute(context.Context, resource.Resource) (json.RawMessage, error) {
	return nil, ErrWaiting
}

// signalFixture is chaosFixture with a recording bus attached to the worker, so a
// test can assert on what the worker published rather than on what it did not.
func signalFixture(t *testing.T, exec StepExecutor) (*recordingBus, resource.Store, *Reconciler, *Worker) {
	t.Helper()
	b := &recordingBus{}
	_, _, store, gr, w := chaosFixture(t, exec, 5, time.Second, time.Minute)
	WithBus(b)(w)
	return b, store, gr, w
}

// goalID reads the id of the goal the step belongs to, which is the payload every
// step signal must carry: a subscriber keys on it to know which goal advanced, and
// a signal naming the wrong resource wakes the wrong watcher.
func goalID(ctx context.Context, t *testing.T, store resource.Store, ref reconcile.Ref) string {
	t.Helper()
	r, err := store.Get(ctx, ref.Kind, ref.Scope, ref.Name)
	if err != nil {
		t.Fatal(err)
	}
	return r.ID
}

// TestWorkerSignalsStepCompletion pins the bus publish on the success path. A
// worker that completes a step without announcing it leaves every subscriber (the
// reconciler's wake path, any watcher) blind until the next poll, and no other
// test fails when the publish is deleted.
func TestWorkerSignalsStepCompletion(t *testing.T) {
	ctx := context.Background()
	b, store, gr, w := signalFixture(t, &recoverAfter{})
	ref := dispatchGoal(t, store, gr)
	want := goalID(ctx, t, store, ref)

	mustProcess(ctx, t, w)

	msgs := b.published(StepSubject)
	if len(msgs) != 1 {
		t.Fatalf("a completed step published %d messages on %s, want exactly 1", len(msgs), StepSubject)
	}
	if got := string(msgs[0].Payload); got != want {
		t.Fatalf("step signal carries payload %q, want the goal id %q", got, want)
	}
}

// TestWorkerSignalsWaitingStep pins the publish on the ErrWaiting path. A step that
// parks on external state has released its job, so the only way anything learns it
// moved is the signal; dropping it here strands the goal until a poll.
func TestWorkerSignalsWaitingStep(t *testing.T) {
	ctx := context.Background()
	b, store, gr, w := signalFixture(t, waitingExec{})
	ref := dispatchGoal(t, store, gr)
	want := goalID(ctx, t, store, ref)

	mustProcess(ctx, t, w)

	msgs := b.published(StepSubject)
	if len(msgs) != 1 {
		t.Fatalf("a waiting step published %d messages on %s, want exactly 1", len(msgs), StepSubject)
	}
	if got := string(msgs[0].Payload); got != want {
		t.Fatalf("waiting-step signal carries payload %q, want the goal id %q", got, want)
	}
}

// TestWorkerDoesNotSignalRetryableFailure pins the other side of the rule: a
// transient failure re-pends the job rather than finishing it, so the step has not
// moved and must not announce that it has. Signalling here would wake every
// subscriber on every retry of a flapping step.
func TestWorkerDoesNotSignalRetryableFailure(t *testing.T) {
	ctx := context.Background()
	b, store, gr, w := signalFixture(t, failingExec{})
	dispatchGoal(t, store, gr)

	mustProcess(ctx, t, w) // claims, runs, fails transiently, re-pends

	if msgs := b.published(StepSubject); len(msgs) != 0 {
		t.Fatalf("a retryable failure published %d messages on %s, want none", len(msgs), StepSubject)
	}
}

// TestWorkerWithoutBusCompletesStep keeps the bus optional: a worker built without
// one still completes its step, because the signal is a notification and never a
// precondition.
func TestWorkerWithoutBusCompletesStep(t *testing.T) {
	ctx := context.Background()
	_, _, store, gr, w := chaosFixture(t, &recoverAfter{}, 5, time.Second, time.Minute)
	ref := dispatchGoal(t, store, gr)

	mustProcess(ctx, t, w) // no bus wired: must not panic and must still complete

	if got := phaseOf(ctx, t, store, ref); got == PhaseStalled {
		t.Fatalf("step stalled with no bus attached, phase %v", got)
	}
}

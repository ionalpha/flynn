package jobs_test

import (
	"context"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/internal/testkit"
	"github.com/ionalpha/flynn/jobs"
)

// The queue's retry, backoff and eviction rules only matter when something
// downstream is failing, so these tests hold a fault plan against the queue port
// itself (testkit.FaultyQueue) and drive it with the same claim-run-settle policy
// a real worker applies. Every fault is deterministic and the clock is manual, so
// each case replays exactly.

const (
	chaosLease   = time.Minute
	chaosBase    = time.Second
	chaosCeiling = 8 * time.Second
)

var (
	errTransient = fault.New(fault.Transient, "flaky_dep", "try again")
	errTerminal  = fault.New(fault.Terminal, "bad_key", "credential rejected")
	errStore     = fault.New(fault.Transient, "store_down", "queue store unavailable")
)

// newChaosQueue builds a memory queue behind the given fault plans, on a manual
// clock shared with the caller so lease expiry and backoff are exact.
func newChaosQueue(t *testing.T, faults testkit.QueueFaults) (jobs.Queue, *clock.Manual) {
	t.Helper()
	clk := clock.NewManual(time.Unix(1_700_000_000, 0).UTC())
	q := testkit.FaultyQueue(jobs.NewMemory(jobs.WithClock(clk)), faults)
	t.Cleanup(func() { _ = q.Close() })
	return q, clk
}

// processOnce claims at most one ready job and settles it exactly as goal.Worker
// does: complete on success, fail permanently on a non-transient cause, and fail
// with an exponential backoff otherwise. It reports whether a job was claimed
// along with the error the queue returned while settling it, so a case can assert
// on a settle that never reached the store.
func processOnce(ctx context.Context, q jobs.Queue, clk *clock.Manual, run func(jobs.Job) error) (claimed bool, err error) {
	got, err := q.Claim(ctx, jobs.ClaimParams{Limit: 1, LeaseFor: int64(chaosLease)})
	if err != nil || len(got) == 0 {
		return false, err
	}
	j := got[0]
	cause := run(j)
	switch {
	case cause == nil:
		return true, q.Complete(ctx, j.ID)
	case fault.Classify(cause) != fault.Transient:
		return true, q.Fail(ctx, j.ID, cause.Error(), -1)
	default:
		retryAt := clk.Now().UnixNano() + jobs.Backoff(j.Attempt, int64(chaosBase), int64(chaosCeiling))
		return true, q.Fail(ctx, j.ID, cause.Error(), retryAt)
	}
}

func mustEnqueue(t *testing.T, q jobs.Queue, maxAttempts int) jobs.Job {
	t.Helper()
	j, err := q.Enqueue(context.Background(), jobs.EnqueueParams{Kind: "step", MaxAttempts: maxAttempts})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	return j
}

func mustGet(t *testing.T, q jobs.Queue, id string) jobs.Job {
	t.Helper()
	j, err := q.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return j
}

// A Complete that never reaches the store must not lose the job. The work was
// done, but nothing recorded it, which is indistinguishable from a worker that
// died a microsecond before completing: the lease lapses and the job is handed
// out again. At-least-once is exactly this, and it is the store failure, not any
// bookkeeping the worker managed to do, that has to carry it.
func TestCompleteThatNeverLandsRedeliversTheJob(t *testing.T) {
	ctx := context.Background()
	q, clk := newChaosQueue(t, testkit.QueueFaults{Complete: testkit.FailFirst(1, errStore)})
	j := mustEnqueue(t, q, 5)

	runs := 0
	claimed, err := processOnce(ctx, q, clk, func(jobs.Job) error { runs++; return nil })
	if !claimed {
		t.Fatal("first pass claimed nothing")
	}
	if err == nil {
		t.Fatal("Complete should have failed against the injected store fault")
	}
	if got := mustGet(t, q, j.ID); got.State != jobs.StateRunning {
		t.Fatalf("state after a lost Complete = %q, want running (the lease must carry it)", got.State)
	}

	// Nothing is claimable while the lease is alive: a lost Complete must not let a
	// second worker pick the job up concurrently.
	if claimed, err := processOnce(ctx, q, clk, func(jobs.Job) error { return nil }); claimed || err != nil {
		t.Fatalf("job reclaimed under a live lease (claimed=%v, err=%v)", claimed, err)
	}

	clk.Advance(chaosLease)
	claimed, err = processOnce(ctx, q, clk, func(jobs.Job) error { runs++; return nil })
	if !claimed || err != nil {
		t.Fatalf("second pass: claimed=%v err=%v, want claimed with no error", claimed, err)
	}
	if got := mustGet(t, q, j.ID); got.State != jobs.StateDone {
		t.Fatalf("state = %q, want done", got.State)
	}
	if runs != 2 {
		t.Fatalf("ran %d times, want 2 (at-least-once redelivery)", runs)
	}
}

// A Fail that never reaches the store must not buy the job unlimited retries. The
// reclaim after the lease lapses spends an attempt exactly as a crash would, so a
// store that can never record a failure still evicts the job at its ceiling rather
// than cycling it forever.
func TestFailThatNeverLandsStillRespectsTheAttemptCeiling(t *testing.T) {
	ctx := context.Background()
	const maxAttempts = 3
	q, clk := newChaosQueue(t, testkit.QueueFaults{Fail: testkit.Always(errStore)})
	j := mustEnqueue(t, q, maxAttempts)

	attempts := 0
	for range maxAttempts {
		claimed, err := processOnce(ctx, q, clk, func(jobs.Job) error { attempts++; return errTransient })
		if !claimed {
			t.Fatalf("attempt %d claimed nothing", attempts)
		}
		if err == nil {
			t.Fatalf("attempt %d: Fail should have failed against the injected store fault", attempts)
		}
		if got := mustGet(t, q, j.ID); got.Attempt != attempts {
			t.Fatalf("Attempt = %d, want %d", got.Attempt, attempts)
		}
		clk.Advance(chaosLease) // the worker looks dead; its lease lapses
	}
	if attempts != maxAttempts {
		t.Fatalf("ran %d attempts, want %d", attempts, maxAttempts)
	}

	// The next Claim reaps the exhausted job rather than handing it out again.
	if claimed, err := processOnce(ctx, q, clk, func(jobs.Job) error { attempts++; return errTransient }); claimed || err != nil {
		t.Fatalf("exhausted job was reclaimed (claimed=%v, err=%v)", claimed, err)
	}
	got := mustGet(t, q, j.ID)
	if got.State != jobs.StateDead {
		t.Fatalf("state = %q, want dead", got.State)
	}
	if got.Attempt != maxAttempts {
		t.Fatalf("Attempt = %d, want exactly %d (an unrecordable failure must not retry past the ceiling)", got.Attempt, maxAttempts)
	}
}

// A persistently failing job must back off between attempts, cap that backoff, and
// die at its ceiling. Retrying with no delay would burn the whole attempt budget in
// microseconds against a dependency that is already struggling.
func TestPersistentFailureBacksOffThenDies(t *testing.T) {
	ctx := context.Background()
	const maxAttempts = 6
	q, clk := newChaosQueue(t, testkit.QueueFaults{})
	j := mustEnqueue(t, q, maxAttempts)

	want := []time.Duration{chaosBase, 2 * chaosBase, 4 * chaosBase, chaosCeiling, chaosCeiling}
	var prev time.Duration
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		claimed, err := processOnce(ctx, q, clk, func(jobs.Job) error { return errTransient })
		if !claimed || err != nil {
			t.Fatalf("attempt %d: claimed=%v err=%v", attempt, claimed, err)
		}
		got := mustGet(t, q, j.ID)
		if attempt == maxAttempts {
			break // the final failure deads the job; there is no delay to check
		}
		if got.State != jobs.StatePending {
			t.Fatalf("attempt %d: state = %q, want pending", attempt, got.State)
		}

		delay := time.Duration(got.RunAt - clk.Now().UnixNano())
		if delay != want[attempt-1] {
			t.Fatalf("attempt %d: backoff = %v, want %v", attempt, delay, want[attempt-1])
		}
		if delay < prev {
			t.Fatalf("attempt %d: backoff %v shrank from %v", attempt, delay, prev)
		}
		if delay > chaosCeiling {
			t.Fatalf("attempt %d: backoff %v exceeds the ceiling %v", attempt, delay, chaosCeiling)
		}
		prev = delay

		// One nanosecond short of RunAt the job is still not claimable: the backoff
		// is enforced by the queue, not merely recorded on the job.
		clk.Advance(delay - 1)
		if claimed, err := processOnce(ctx, q, clk, func(jobs.Job) error { return nil }); claimed || err != nil {
			t.Fatalf("attempt %d: job claimed before its backoff elapsed (claimed=%v, err=%v)", attempt, claimed, err)
		}
		clk.Advance(1)
	}

	got := mustGet(t, q, j.ID)
	if got.State != jobs.StateDead {
		t.Fatalf("state = %q after %d failures, want dead", got.State, maxAttempts)
	}
	clk.Advance(time.Hour)
	if claimed, _ := processOnce(ctx, q, clk, func(jobs.Job) error { return nil }); claimed {
		t.Fatal("a dead job was reclaimed")
	}
}

// A cause that cannot succeed on retry (a rejected credential, a malformed
// payload) must evict the job at once. Spending 25 attempts and an hour of backoff
// on a bad API key is how a stalled goal takes an hour to surface instead of a
// second.
func TestTerminalFailureEvictsWithoutBurningAttempts(t *testing.T) {
	ctx := context.Background()
	q, clk := newChaosQueue(t, testkit.QueueFaults{})
	j := mustEnqueue(t, q, 25)

	runs := 0
	claimed, err := processOnce(ctx, q, clk, func(jobs.Job) error { runs++; return errTerminal })
	if !claimed || err != nil {
		t.Fatalf("claimed=%v err=%v", claimed, err)
	}
	got := mustGet(t, q, j.ID)
	if got.State != jobs.StateDead {
		t.Fatalf("state = %q, want dead on the first terminal failure", got.State)
	}
	if got.Attempt != 1 {
		t.Fatalf("Attempt = %d, want 1", got.Attempt)
	}

	clk.Advance(time.Hour)
	if claimed, _ := processOnce(ctx, q, clk, func(jobs.Job) error { runs++; return nil }); claimed {
		t.Fatal("a terminally failed job was retried")
	}
	if runs != 1 {
		t.Fatalf("ran %d times, want 1", runs)
	}
}

// A store that fails every other read must not drop or duplicate work. The worker
// sees an error instead of a job, and the only correct response is to come back:
// every enqueued job still runs exactly once.
func TestFlakyClaimDrainsEveryJobExactlyOnce(t *testing.T) {
	ctx := context.Background()
	const n = 8
	q, clk := newChaosQueue(t, testkit.QueueFaults{Claim: testkit.FailEvery(2, errStore)})

	ids := make([]string, 0, n)
	for range n {
		ids = append(ids, mustEnqueue(t, q, 3).ID)
	}

	seen := make(map[string]int, n)
	claimErrs := 0
	// Bounded: every pass either claims a job or absorbs one injected read fault.
	for pass := 0; pass < 4*n && len(seen) < n; pass++ {
		claimed, err := processOnce(ctx, q, clk, func(j jobs.Job) error { seen[j.ID]++; return nil })
		if err != nil {
			claimErrs++
			continue
		}
		if !claimed {
			t.Fatalf("pass %d: nothing claimed and no fault, with %d jobs left", pass, n-len(seen))
		}
	}
	if claimErrs == 0 {
		t.Fatal("no Claim fault was injected; the test proved nothing")
	}
	if len(seen) != n {
		t.Fatalf("ran %d distinct jobs, want %d", len(seen), n)
	}
	for _, id := range ids {
		if seen[id] != 1 {
			t.Fatalf("job %s ran %d times, want exactly 1", id, seen[id])
		}
		if got := mustGet(t, q, id); got.State != jobs.StateDone {
			t.Fatalf("job %s state = %q, want done", id, got.State)
		}
	}
}

// Startup crash recovery runs against a store that may itself be failing. A
// Recover that errors must leave the queue untouched and stay safe to call again:
// the retry recovers the same job, and recovery never spends an attempt of its own.
func TestRecoverIsRetryableAndSpendsNoAttempt(t *testing.T) {
	ctx := context.Background()
	q, clk := newChaosQueue(t, testkit.QueueFaults{Recover: testkit.FailFirst(1, errStore)})
	j := mustEnqueue(t, q, 3)

	// A worker claims the job, then dies without settling it: no Complete, no Fail,
	// just a lease held by a process that is gone.
	claimed, err := q.Claim(ctx, jobs.ClaimParams{Limit: 1, LeaseFor: int64(chaosLease)})
	if err != nil || len(claimed) != 1 {
		t.Fatalf("setup claim: got %d jobs, err %v", len(claimed), err)
	}
	before := mustGet(t, q, j.ID)

	if _, err := q.Recover(ctx); err == nil {
		t.Fatal("Recover should have failed against the injected store fault")
	}
	if got := mustGet(t, q, j.ID); got.State != before.State || got.Attempt != before.Attempt || got.LeaseExpires != before.LeaseExpires {
		t.Fatalf("a failed Recover mutated the job:\n got %+v\nwant %+v", got, before)
	}

	n, err := q.Recover(ctx)
	if err != nil {
		t.Fatalf("Recover retry: %v", err)
	}
	if n != 1 {
		t.Fatalf("recovered %d jobs, want 1", n)
	}

	// The reclaim is immediate (no lease to wait out) and costs one attempt, the
	// same as any claim: recovery expires the lease, it does not spend an attempt
	// of its own.
	ran, err := processOnce(ctx, q, clk, func(jobs.Job) error { return nil })
	if !ran || err != nil {
		t.Fatalf("post-recover claim: claimed=%v err=%v", ran, err)
	}
	got := mustGet(t, q, j.ID)
	if got.State != jobs.StateDone {
		t.Fatalf("state = %q, want done", got.State)
	}
	if got.Attempt != before.Attempt+1 {
		t.Fatalf("Attempt = %d, want %d (recovery itself must not spend an attempt)", got.Attempt, before.Attempt+1)
	}
}

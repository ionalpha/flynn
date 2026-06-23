// Package jobstest is the conformance suite for jobs.Queue. Every backend (the
// in-memory reference, the durable SQLite default, a River or NATS adapter) runs
// RunSuite and must behave identically, so a durable or distributed queue is held
// to the exact enqueue, lease, retry, and lifecycle contract of the reference
// MemoryQueue rather than re-tested by hand.
//
// The suite drives time through a clock.Manual so lease expiry and scheduled
// RunAt are exercised deterministically, with no real sleeping.
package jobstest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/jobs"
)

// Harness is a queue paired with the manual clock that drives it, so the suite
// can advance time to trigger scheduling and lease expiry.
type Harness struct {
	Queue jobs.Queue
	Clock *clock.Manual
}

// RunSuite runs the full jobs.Queue contract against queues built by newQueue.
// Each subtest gets a fresh harness, with its queue closed at the end.
func RunSuite(t *testing.T, newQueue func() Harness) {
	t.Helper()
	t.Run("EnqueueAppliesDefaults", func(t *testing.T) { testEnqueueDefaults(t, newQueue()) })
	t.Run("EnqueueRequiresKind", func(t *testing.T) { testEnqueueRequiresKind(t, newQueue()) })
	t.Run("ClaimLeasesReadyJob", func(t *testing.T) { testClaimLeases(t, newQueue()) })
	t.Run("ClaimIsExclusive", func(t *testing.T) { testClaimExclusive(t, newQueue()) })
	t.Run("ClaimRespectsRunAt", func(t *testing.T) { testClaimRunAt(t, newQueue()) })
	t.Run("ClaimRespectsLimit", func(t *testing.T) { testClaimLimit(t, newQueue()) })
	t.Run("ClaimReclaimsExpiredLease", func(t *testing.T) { testClaimReclaims(t, newQueue()) })
	t.Run("ExhaustedLeaseTimesOutToDead", func(t *testing.T) { testClaimTimesOut(t, newQueue()) })
	t.Run("ClaimIsolatesQueues", func(t *testing.T) { testClaimQueueIsolation(t, newQueue()) })
	t.Run("ClaimOrdersByRunAt", func(t *testing.T) { testClaimOrdering(t, newQueue()) })
	t.Run("CompleteMarksDone", func(t *testing.T) { testComplete(t, newQueue()) })
	t.Run("FailRetriesThenDies", func(t *testing.T) { testFailRetriesThenDies(t, newQueue()) })
	t.Run("CompleteAndFailGuardState", func(t *testing.T) { testGuards(t, newQueue()) })
	t.Run("GetUnknownIsNotFound", func(t *testing.T) { testGetNotFound(t, newQueue()) })
}

const lease = int64(time.Minute)

func ctx() context.Context { return context.Background() }

func mustEnqueue(t *testing.T, q jobs.Queue, p jobs.EnqueueParams) jobs.Job {
	t.Helper()
	j, err := q.Enqueue(ctx(), p)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	return j
}

func claimOne(t *testing.T, h Harness, queue string) (jobs.Job, bool) {
	t.Helper()
	got, err := h.Queue.Claim(ctx(), jobs.ClaimParams{Queue: queue, Limit: 1, LeaseFor: lease})
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(got) == 0 {
		return jobs.Job{}, false
	}
	return got[0], true
}

func testEnqueueDefaults(t *testing.T, h Harness) {
	defer func() { _ = h.Queue.Close() }()
	j := mustEnqueue(t, h.Queue, jobs.EnqueueParams{Kind: "send.email"})
	if j.ID == "" {
		t.Fatal("Enqueue did not assign an ID")
	}
	if j.Queue != jobs.DefaultQueue {
		t.Errorf("Queue = %q, want %q", j.Queue, jobs.DefaultQueue)
	}
	if j.State != jobs.StatePending {
		t.Errorf("State = %q, want pending", j.State)
	}
	if j.MaxAttempts != jobs.DefaultMaxAttempts {
		t.Errorf("MaxAttempts = %d, want %d", j.MaxAttempts, jobs.DefaultMaxAttempts)
	}
	if j.Attempt != 0 {
		t.Errorf("Attempt = %d, want 0 before any claim", j.Attempt)
	}
}

func testEnqueueRequiresKind(t *testing.T, h Harness) {
	defer func() { _ = h.Queue.Close() }()
	if _, err := h.Queue.Enqueue(ctx(), jobs.EnqueueParams{}); !errors.Is(err, jobs.ErrInvalidJob) {
		t.Fatalf("Enqueue with empty Kind = %v, want ErrInvalidJob", err)
	}
}

func testClaimLeases(t *testing.T, h Harness) {
	defer func() { _ = h.Queue.Close() }()
	enq := mustEnqueue(t, h.Queue, jobs.EnqueueParams{Kind: "k"})
	j, ok := claimOne(t, h, "")
	if !ok {
		t.Fatal("Claim returned no job for a ready queue")
	}
	if j.ID != enq.ID {
		t.Errorf("claimed %q, want %q", j.ID, enq.ID)
	}
	if j.State != jobs.StateRunning {
		t.Errorf("State = %q, want running", j.State)
	}
	if j.Attempt != 1 {
		t.Errorf("Attempt = %d, want 1 after first claim", j.Attempt)
	}
	if j.LeaseExpires <= h.Clock.Now().UnixNano() {
		t.Errorf("LeaseExpires not in the future after claim")
	}
}

func testClaimExclusive(t *testing.T, h Harness) {
	defer func() { _ = h.Queue.Close() }()
	mustEnqueue(t, h.Queue, jobs.EnqueueParams{Kind: "k"})
	if _, ok := claimOne(t, h, ""); !ok {
		t.Fatal("first claim got nothing")
	}
	// While leased (clock not advanced past the lease), no second worker may
	// claim the same job.
	if j, ok := claimOne(t, h, ""); ok {
		t.Fatalf("second claim got leased job %q, want exclusivity", j.ID)
	}
}

func testClaimRunAt(t *testing.T, h Harness) {
	defer func() { _ = h.Queue.Close() }()
	future := h.Clock.Now().Add(time.Hour).UnixNano()
	mustEnqueue(t, h.Queue, jobs.EnqueueParams{Kind: "k", RunAt: future})
	if _, ok := claimOne(t, h, ""); ok {
		t.Fatal("claimed a job scheduled in the future")
	}
	h.Clock.Advance(2 * time.Hour)
	if _, ok := claimOne(t, h, ""); !ok {
		t.Fatal("did not claim a job after its RunAt arrived")
	}
}

func testClaimLimit(t *testing.T, h Harness) {
	defer func() { _ = h.Queue.Close() }()
	for i := 0; i < 5; i++ {
		mustEnqueue(t, h.Queue, jobs.EnqueueParams{Kind: "k"})
	}
	got, err := h.Queue.Claim(ctx(), jobs.ClaimParams{Limit: 3, LeaseFor: lease})
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("claimed %d jobs, want 3 (limit)", len(got))
	}
}

func testClaimReclaims(t *testing.T, h Harness) {
	defer func() { _ = h.Queue.Close() }()
	mustEnqueue(t, h.Queue, jobs.EnqueueParams{Kind: "k"})
	first, _ := claimOne(t, h, "")
	// The worker crashes without completing. After the lease expires the job must
	// be claimable again, with a higher attempt count.
	h.Clock.Advance(2 * time.Minute)
	second, ok := claimOne(t, h, "")
	if !ok {
		t.Fatal("expired-lease job was not reclaimed")
	}
	if second.ID != first.ID {
		t.Fatalf("reclaimed %q, want the same job %q", second.ID, first.ID)
	}
	if second.Attempt != 2 {
		t.Errorf("Attempt = %d after reclaim, want 2", second.Attempt)
	}
}

func testClaimTimesOut(t *testing.T, h Harness) {
	defer func() { _ = h.Queue.Close() }()
	// One attempt allowed. The worker claims it then crashes (never completes).
	j := mustEnqueue(t, h.Queue, jobs.EnqueueParams{Kind: "k", MaxAttempts: 1})
	claimed, ok := claimOne(t, h, "")
	if !ok {
		t.Fatal("first claim got nothing")
	}
	if claimed.Attempt != 1 {
		t.Fatalf("Attempt = %d, want 1", claimed.Attempt)
	}
	// The lease expires with no attempts left: the next claim reaps it to dead
	// rather than re-leasing it past MaxAttempts.
	h.Clock.Advance(2 * time.Minute)
	if reclaimed, ok := claimOne(t, h, ""); ok {
		t.Fatalf("re-leased an exhausted job %q past MaxAttempts", reclaimed.ID)
	}
	got, err := h.Queue.Get(ctx(), j.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != jobs.StateDead {
		t.Fatalf("State = %q, want dead after exhausted lease timeout", got.State)
	}
	if got.Attempt > got.MaxAttempts {
		t.Fatalf("Attempt %d exceeds MaxAttempts %d", got.Attempt, got.MaxAttempts)
	}
}

func testClaimQueueIsolation(t *testing.T, h Harness) {
	defer func() { _ = h.Queue.Close() }()
	mustEnqueue(t, h.Queue, jobs.EnqueueParams{Kind: "k", Queue: "emails"})
	if _, ok := claimOne(t, h, "reports"); ok {
		t.Fatal("claimed a job from a different queue")
	}
	if _, ok := claimOne(t, h, "emails"); !ok {
		t.Fatal("did not claim from the job's own queue")
	}
}

func testClaimOrdering(t *testing.T, h Harness) {
	defer func() { _ = h.Queue.Close() }()
	now := h.Clock.Now().UnixNano()
	later := mustEnqueue(t, h.Queue, jobs.EnqueueParams{Kind: "k", RunAt: now + int64(time.Second)})
	_ = later
	earlier := mustEnqueue(t, h.Queue, jobs.EnqueueParams{Kind: "k", RunAt: now})
	h.Clock.Advance(time.Hour) // both are now due
	j, ok := claimOne(t, h, "")
	if !ok {
		t.Fatal("claim got nothing")
	}
	if j.ID != earlier.ID {
		t.Errorf("claimed %q first, want the earlier-RunAt job %q", j.ID, earlier.ID)
	}
}

func testComplete(t *testing.T, h Harness) {
	defer func() { _ = h.Queue.Close() }()
	mustEnqueue(t, h.Queue, jobs.EnqueueParams{Kind: "k"})
	j, _ := claimOne(t, h, "")
	if err := h.Queue.Complete(ctx(), j.ID); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	got, err := h.Queue.Get(ctx(), j.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != jobs.StateDone {
		t.Errorf("State = %q, want done", got.State)
	}
	// A completed job is not reclaimable.
	h.Clock.Advance(2 * time.Minute)
	if _, ok := claimOne(t, h, ""); ok {
		t.Fatal("claimed a completed job")
	}
}

func testFailRetriesThenDies(t *testing.T, h Harness) {
	defer func() { _ = h.Queue.Close() }()
	mustEnqueue(t, h.Queue, jobs.EnqueueParams{Kind: "k", MaxAttempts: 2})

	// Attempt 1 fails with a retry scheduled now: the job returns to pending.
	j, _ := claimOne(t, h, "")
	if err := h.Queue.Fail(ctx(), j.ID, "boom", h.Clock.Now().UnixNano()); err != nil {
		t.Fatalf("Fail #1: %v", err)
	}
	got, _ := h.Queue.Get(ctx(), j.ID)
	if got.State != jobs.StatePending {
		t.Fatalf("after fail #1 State = %q, want pending (retry remains)", got.State)
	}
	if got.LastError != "boom" {
		t.Errorf("LastError = %q, want boom", got.LastError)
	}

	// Attempt 2 fails: attempts are exhausted, so the job goes dead.
	j2, ok := claimOne(t, h, "")
	if !ok {
		t.Fatal("retry was not reclaimable")
	}
	if err := h.Queue.Fail(ctx(), j2.ID, "boom again", h.Clock.Now().UnixNano()); err != nil {
		t.Fatalf("Fail #2: %v", err)
	}
	got, _ = h.Queue.Get(ctx(), j2.ID)
	if got.State != jobs.StateDead {
		t.Fatalf("after fail #2 State = %q, want dead", got.State)
	}
	// A dead job is never reclaimed.
	h.Clock.Advance(2 * time.Minute)
	if _, ok := claimOne(t, h, ""); ok {
		t.Fatal("claimed a dead job")
	}
}

func testGuards(t *testing.T, h Harness) {
	defer func() { _ = h.Queue.Close() }()
	// A pending (never-claimed) job cannot be completed or failed.
	j := mustEnqueue(t, h.Queue, jobs.EnqueueParams{Kind: "k"})
	if err := h.Queue.Complete(ctx(), j.ID); !errors.Is(err, jobs.ErrNotRunning) {
		t.Errorf("Complete pending = %v, want ErrNotRunning", err)
	}
	if err := h.Queue.Fail(ctx(), j.ID, "x", 0); !errors.Is(err, jobs.ErrNotRunning) {
		t.Errorf("Fail pending = %v, want ErrNotRunning", err)
	}
	// Unknown IDs are ErrNotFound.
	if err := h.Queue.Complete(ctx(), "nope"); !errors.Is(err, jobs.ErrNotFound) {
		t.Errorf("Complete unknown = %v, want ErrNotFound", err)
	}
}

func testGetNotFound(t *testing.T, h Harness) {
	defer func() { _ = h.Queue.Close() }()
	if _, err := h.Queue.Get(ctx(), "missing"); !errors.Is(err, jobs.ErrNotFound) {
		t.Fatalf("Get unknown = %v, want ErrNotFound", err)
	}
}

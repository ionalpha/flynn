package jobs

import (
	"context"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
)

// These are white-box tests of the claim index: Claim scans q.active only, so
// the structural guarantee that terminal jobs leave that index IS the
// performance guarantee that claim cost does not grow with the done/dead
// backlog (the alloc ceilings and benchmarks measure the same thing from the
// outside).

func activeCount(q *MemoryQueue) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := 0
	for _, byID := range q.active {
		n += len(byID)
	}
	return n
}

func indexHarness(t *testing.T) (*MemoryQueue, *clock.Manual, context.Context) {
	t.Helper()
	m := clock.NewManual(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	return NewMemory(WithClock(m)), m, context.Background()
}

func claimIndexed(t *testing.T, q *MemoryQueue) Job {
	t.Helper()
	got, err := q.Claim(context.Background(), ClaimParams{Limit: 1, LeaseFor: int64(time.Minute)})
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("claimed %d jobs, want 1", len(got))
	}
	return got[0]
}

func TestCompletedJobLeavesClaimIndex(t *testing.T) {
	q, _, ctx := indexHarness(t)
	j, err := q.Enqueue(ctx, EnqueueParams{Kind: "k"})
	if err != nil {
		t.Fatal(err)
	}
	if activeCount(q) != 1 {
		t.Fatalf("active = %d after enqueue, want 1", activeCount(q))
	}
	c := claimIndexed(t, q)
	if activeCount(q) != 1 {
		t.Fatalf("active = %d while running, want 1 (a lease can expire and be reclaimed)", activeCount(q))
	}
	if err := q.Complete(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if activeCount(q) != 0 {
		t.Fatalf("active = %d after Complete, want 0: a done job must leave the claim path", activeCount(q))
	}
	// Retention for inspection is unchanged: the job is still Get-able.
	got, err := q.Get(ctx, j.ID)
	if err != nil {
		t.Fatalf("Get after Complete: %v", err)
	}
	if got.State != StateDone {
		t.Fatalf("State = %q, want done", got.State)
	}
}

func TestDeadJobLeavesClaimIndex(t *testing.T) {
	q, _, ctx := indexHarness(t)
	if _, err := q.Enqueue(ctx, EnqueueParams{Kind: "k", MaxAttempts: 5}); err != nil {
		t.Fatal(err)
	}
	c := claimIndexed(t, q)
	// Permanent failure (negative retryAt) deads the job at once.
	if err := q.Fail(ctx, c.ID, "terminal cause", -1); err != nil {
		t.Fatal(err)
	}
	if activeCount(q) != 0 {
		t.Fatalf("active = %d after permanent Fail, want 0: a dead job must leave the claim path", activeCount(q))
	}
}

func TestRetryingJobStaysInClaimIndex(t *testing.T) {
	q, m, ctx := indexHarness(t)
	if _, err := q.Enqueue(ctx, EnqueueParams{Kind: "k", MaxAttempts: 3}); err != nil {
		t.Fatal(err)
	}
	c := claimIndexed(t, q)
	if err := q.Fail(ctx, c.ID, "boom", m.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
	if activeCount(q) != 1 {
		t.Fatalf("active = %d after retryable Fail, want 1: a re-pended job must remain claimable", activeCount(q))
	}
	if got := claimIndexed(t, q); got.ID != c.ID || got.Attempt != 2 {
		t.Fatalf("reclaim got %q attempt %d, want %q attempt 2", got.ID, got.Attempt, c.ID)
	}
}

func TestReapedTimeoutLeavesClaimIndex(t *testing.T) {
	q, m, ctx := indexHarness(t)
	if _, err := q.Enqueue(ctx, EnqueueParams{Kind: "k", MaxAttempts: 1}); err != nil {
		t.Fatal(err)
	}
	claimIndexed(t, q)
	// The lease expires with no attempts left; the next Claim reaps it to dead
	// and must also drop it from the index.
	m.Advance(2 * time.Minute)
	got, err := q.Claim(ctx, ClaimParams{Limit: 1, LeaseFor: int64(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("claimed %d jobs, want 0 (job was exhausted)", len(got))
	}
	if activeCount(q) != 0 {
		t.Fatalf("active = %d after timeout reap, want 0", activeCount(q))
	}
}

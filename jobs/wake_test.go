package jobs_test

import (
	"context"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/jobs"
)

// The Waker contract on the reference queue: Enqueue and a retryable Fail
// signal Ready, terminal transitions do not, and signals coalesce into the one
// buffered slot. The channel is buffered, so every assertion here is a
// non-blocking receive: no sleeping, no flakiness.

func drained(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return false
	default:
		return true
	}
}

func TestMemoryQueueSignalsReadyOnEnqueue(t *testing.T) {
	q := jobs.NewMemory()
	ready := q.Ready()
	if !drained(ready) {
		t.Fatal("Ready signalled before any write")
	}
	if _, err := q.Enqueue(context.Background(), jobs.EnqueueParams{Kind: "k"}); err != nil {
		t.Fatal(err)
	}
	if drained(ready) {
		t.Fatal("Enqueue did not signal Ready")
	}
	// Coalescing: several enqueues while nobody listens collapse to one signal.
	for range 3 {
		if _, err := q.Enqueue(context.Background(), jobs.EnqueueParams{Kind: "k"}); err != nil {
			t.Fatal(err)
		}
	}
	if drained(ready) {
		t.Fatal("coalesced enqueues left no signal")
	}
	if !drained(ready) {
		t.Fatal("coalesced enqueues left more than one signal")
	}
}

func TestMemoryQueueSignalsReadyOnRetryOnly(t *testing.T) {
	m := clock.NewManual(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	q := jobs.NewMemory(jobs.WithClock(m))
	ctx := context.Background()
	ready := q.Ready()

	enqueueClaim := func() jobs.Job {
		t.Helper()
		if _, err := q.Enqueue(ctx, jobs.EnqueueParams{Kind: "k", MaxAttempts: 5}); err != nil {
			t.Fatal(err)
		}
		<-ready // drain the enqueue signal so the Fail behaviour is isolated
		got, err := q.Claim(ctx, jobs.ClaimParams{Limit: 1, LeaseFor: int64(time.Minute)})
		if err != nil || len(got) != 1 {
			t.Fatalf("Claim = %v jobs, err %v", len(got), err)
		}
		return got[0]
	}

	// A retryable failure re-pends the job and must wake a worker.
	j := enqueueClaim()
	if err := q.Fail(ctx, j.ID, "boom", m.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
	if drained(ready) {
		t.Fatal("retryable Fail did not signal Ready")
	}

	// A permanent failure produces no claimable work and must not signal.
	j = enqueueClaim()
	if err := q.Fail(ctx, j.ID, "terminal", -1); err != nil {
		t.Fatal(err)
	}
	if !drained(ready) {
		t.Fatal("permanent Fail signalled Ready with nothing claimable")
	}

	// Complete likewise produces no work and must not signal.
	j = enqueueClaim()
	if err := q.Complete(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	if !drained(ready) {
		t.Fatal("Complete signalled Ready with nothing claimable")
	}
}

package sqlite_test

import (
	"context"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/jobs"
	"github.com/ionalpha/flynn/jobs/jobstest"
	"github.com/ionalpha/flynn/storage/sqlite"
)

// TestJobsConformance holds the durable SQLite queue to the same contract as the
// in-memory reference, so the two backends are interchangeable.
func TestJobsConformance(t *testing.T) {
	jobstest.RunSuite(t, func() jobstest.Harness {
		clk := clock.NewManual(time.Unix(1_700_000_000, 0).UTC())
		store, err := sqlite.Open(context.Background(), ":memory:", sqlite.WithClock(clk))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		t.Cleanup(func() { _ = store.Close() })
		return jobstest.Harness{Queue: store.Jobs(), Clock: clk}
	})
}

// TestJobsSignalReady covers the jobs.Waker capability on the durable queue:
// an in-process enqueue signals the shared ready channel, and every Jobs()
// facade shares that one channel (it lives on the Store), so a worker holding
// one facade is woken by writes through another.
func TestJobsSignalReady(t *testing.T) {
	clk := clock.NewManual(time.Unix(1_700_000_000, 0).UTC())
	store, err := sqlite.Open(context.Background(), ":memory:", sqlite.WithClock(clk))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	q := store.Jobs()
	wk, ok := q.(jobs.Waker)
	if !ok {
		t.Fatal("sqlite queue does not implement jobs.Waker")
	}
	other, ok := store.Jobs().(jobs.Waker)
	if !ok || other.Ready() != wk.Ready() {
		t.Fatal("Jobs() facades do not share one ready channel")
	}

	ctx := context.Background()
	if _, err := q.Enqueue(ctx, jobs.EnqueueParams{Kind: "k", MaxAttempts: 2}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-wk.Ready():
	default:
		t.Fatal("Enqueue did not signal Ready")
	}

	// A retryable failure re-pends the job and signals; the terminal follow-up
	// failure does not.
	claimed, err := q.Claim(ctx, jobs.ClaimParams{Limit: 1, LeaseFor: int64(time.Minute)})
	if err != nil || len(claimed) != 1 {
		t.Fatalf("Claim = %d jobs, err %v", len(claimed), err)
	}
	if err := q.Fail(ctx, claimed[0].ID, "boom", clk.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-wk.Ready():
	default:
		t.Fatal("retryable Fail did not signal Ready")
	}
	claimed, err = q.Claim(ctx, jobs.ClaimParams{Limit: 1, LeaseFor: int64(time.Minute)})
	if err != nil || len(claimed) != 1 {
		t.Fatalf("reclaim = %d jobs, err %v", len(claimed), err)
	}
	if err := q.Fail(ctx, claimed[0].ID, "terminal", -1); err != nil {
		t.Fatal(err)
	}
	select {
	case <-wk.Ready():
		t.Fatal("permanent Fail signalled Ready with nothing claimable")
	default:
	}
}

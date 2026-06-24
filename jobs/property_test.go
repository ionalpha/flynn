package jobs_test

import (
	"context"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/jobs"
)

func newQueue() (jobs.Queue, *clock.Manual) {
	clk := clock.NewManual(time.Unix(1_700_000_000, 0).UTC())
	return jobs.NewMemory(jobs.WithClock(clk)), clk
}

// Property: a job that keeps failing reaches StateDead after exactly MaxAttempts
// attempts and is never claimable again, for any MaxAttempts in [1, 12].
func TestProp_FailExhaustsAfterMaxAttempts(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		q, clk := newQueue()
		defer func() { _ = q.Close() }()
		ctx := context.Background()
		maxAttempts := rapid.IntRange(1, 12).Draw(rt, "maxAttempts")

		j, err := q.Enqueue(ctx, jobs.EnqueueParams{Kind: "k", MaxAttempts: maxAttempts})
		if err != nil {
			rt.Fatalf("Enqueue: %v", err)
		}

		for attempt := 1; attempt <= maxAttempts; attempt++ {
			claimed, err := q.Claim(ctx, jobs.ClaimParams{Limit: 1, LeaseFor: int64(time.Minute)})
			if err != nil {
				rt.Fatalf("Claim: %v", err)
			}
			if len(claimed) != 1 {
				rt.Fatalf("attempt %d: claimed %d jobs, want 1", attempt, len(claimed))
			}
			if claimed[0].Attempt != attempt {
				rt.Fatalf("Attempt = %d, want %d", claimed[0].Attempt, attempt)
			}
			if err := q.Fail(ctx, j.ID, "boom", clk.Now().UnixNano()); err != nil {
				rt.Fatalf("Fail: %v", err)
			}
		}

		got, err := q.Get(ctx, j.ID)
		if err != nil {
			rt.Fatalf("Get: %v", err)
		}
		if got.State != jobs.StateDead {
			rt.Fatalf("State = %q after %d failures, want dead", got.State, maxAttempts)
		}
		// Past any backoff, a dead job is still never reclaimed.
		clk.Advance(time.Hour)
		claimed, err := q.Claim(ctx, jobs.ClaimParams{Limit: 10, LeaseFor: int64(time.Minute)})
		if err != nil {
			rt.Fatalf("Claim: %v", err)
		}
		if len(claimed) != 0 {
			rt.Fatalf("claimed a dead job: %+v", claimed)
		}
	})
}

// Property: across a batch of claims taken at one instant (no lease expiry), each
// ready job is leased to at most one caller. No ID is handed out twice.
func TestProp_ClaimNeverDoubleLeases(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		q, _ := newQueue()
		defer func() { _ = q.Close() }()
		ctx := context.Background()

		n := rapid.IntRange(1, 30).Draw(rt, "jobs")
		for range n {
			if _, err := q.Enqueue(ctx, jobs.EnqueueParams{Kind: "k"}); err != nil {
				rt.Fatalf("Enqueue: %v", err)
			}
		}

		seen := make(map[string]bool)
		// Drain in random-sized batches; the lease is long and the clock never
		// advances, so nothing is ever reclaimable mid-drain.
		for {
			limit := rapid.IntRange(1, 7).Draw(rt, "limit")
			claimed, err := q.Claim(ctx, jobs.ClaimParams{Limit: limit, LeaseFor: int64(time.Hour)})
			if err != nil {
				rt.Fatalf("Claim: %v", err)
			}
			if len(claimed) == 0 {
				break
			}
			for _, j := range claimed {
				if seen[j.ID] {
					rt.Fatalf("job %q leased twice", j.ID)
				}
				seen[j.ID] = true
			}
		}
		if len(seen) != n {
			rt.Fatalf("claimed %d distinct jobs, want all %d", len(seen), n)
		}
	})
}

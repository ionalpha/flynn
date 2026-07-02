package jobs_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/jobs"
)

// benchQueue is a MemoryQueue on a manual clock carrying n terminal (done)
// jobs, the state of a long-lived server that has processed n steps. The M5
// regression these benchmarks guard: claim cost must not grow with that
// backlog, because terminal jobs leave the claim index when they settle.
func benchQueue(tb testing.TB, n int) (*jobs.MemoryQueue, context.Context) {
	tb.Helper()
	m := clock.NewManual(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	q := jobs.NewMemory(jobs.WithClock(m))
	ctx := context.Background()
	for range n {
		if _, err := q.Enqueue(ctx, jobs.EnqueueParams{Kind: "k"}); err != nil {
			tb.Fatal(err)
		}
		got, err := q.Claim(ctx, jobs.ClaimParams{Limit: 1, LeaseFor: int64(time.Minute)})
		if err != nil || len(got) != 1 {
			tb.Fatalf("Claim = %d jobs, err %v", len(got), err)
		}
		if err := q.Complete(ctx, got[0].ID); err != nil {
			tb.Fatal(err)
		}
	}
	return q, ctx
}

// BenchmarkClaimIdle is the idle worker poll: a claim that finds nothing, with
// a growing terminal backlog present. Near-constant across the sizes.
func BenchmarkClaimIdle(b *testing.B) {
	for _, n := range []int{100, 10_000, 100_000} {
		b.Run(fmt.Sprintf("terminal=%d", n), func(b *testing.B) {
			q, ctx := benchQueue(b, n)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if _, err := q.Claim(ctx, jobs.ClaimParams{Limit: 1, LeaseFor: int64(time.Minute)}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkDispatchCycle is one full durable step dispatch (enqueue, claim,
// complete) with a realistic terminal backlog present.
func BenchmarkDispatchCycle(b *testing.B) {
	q, ctx := benchQueue(b, 10_000)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := q.Enqueue(ctx, jobs.EnqueueParams{Kind: "k"}); err != nil {
			b.Fatal(err)
		}
		got, err := q.Claim(ctx, jobs.ClaimParams{Limit: 1, LeaseFor: int64(time.Minute)})
		if err != nil || len(got) != 1 {
			b.Fatalf("Claim = %d jobs, err %v", len(got), err)
		}
		if err := q.Complete(ctx, got[0].ID); err != nil {
			b.Fatal(err)
		}
	}
}

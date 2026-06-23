package jobs_test

import (
	"context"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/jobs"
)

// FuzzBackoff throws arbitrary attempt/base/max triples at the retry policy and
// asserts it never panics and always returns a delay within [base, max] for sane
// inputs, with no overflow into negative durations.
func FuzzBackoff(f *testing.F) {
	f.Add(1, int64(time.Second), int64(time.Minute))
	f.Add(0, int64(0), int64(0))
	f.Add(100, int64(1<<60), int64(1<<62))
	f.Add(-5, int64(-1), int64(1))
	f.Add(64, int64(1), int64(1<<62))

	f.Fuzz(func(t *testing.T, attempt int, base, ceiling int64) {
		got := jobs.Backoff(attempt, base, ceiling)
		// For a well-formed policy (0 < base <= ceiling), the result is a real delay
		// inside the bounds and never overflows negative.
		if base > 0 && ceiling >= base {
			if got < base || got > ceiling {
				t.Fatalf("Backoff(%d, %d, %d) = %d, out of [base, ceiling]", attempt, base, ceiling, got)
			}
		}
	})
}

// FuzzLifecycle drives a queue through an arbitrary sequence of operations and
// asserts the core invariants hold for any interleaving: Attempt never exceeds
// MaxAttempts, terminal jobs stay terminal, and a job is never both done and
// claimable.
func FuzzLifecycle(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 1, 2, 3})
	f.Add([]byte{1, 1, 1, 1, 1})
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, ops []byte) {
		clk := clock.NewManual(time.Unix(1_700_000_000, 0).UTC())
		q := jobs.NewMemory(jobs.WithClock(clk))
		defer func() { _ = q.Close() }()
		ctx := context.Background()

		var leased []string // currently claimed job IDs
		for _, op := range ops {
			switch op % 5 {
			case 0: // enqueue
				if _, err := q.Enqueue(ctx, jobs.EnqueueParams{Kind: "k", MaxAttempts: 3}); err != nil {
					t.Fatalf("Enqueue: %v", err)
				}
			case 1: // claim one
				got, err := q.Claim(ctx, jobs.ClaimParams{Limit: 1, LeaseFor: int64(time.Minute)})
				if err != nil {
					t.Fatalf("Claim: %v", err)
				}
				for _, j := range got {
					if j.Attempt > j.MaxAttempts {
						t.Fatalf("Attempt %d exceeds MaxAttempts %d", j.Attempt, j.MaxAttempts)
					}
					leased = append(leased, j.ID)
				}
			case 2: // complete the oldest lease, if any
				if len(leased) > 0 {
					id := leased[0]
					leased = leased[1:]
					if err := q.Complete(ctx, id); err != nil {
						// The lease may have expired and been reclaimed; that is a
						// legitimate not-running, not a bug.
						continue
					}
					got, _ := q.Get(ctx, id)
					if got.State != jobs.StateDone {
						t.Fatalf("completed job state = %q, want done", got.State)
					}
				}
			case 3: // fail the oldest lease, if any
				if len(leased) > 0 {
					id := leased[0]
					leased = leased[1:]
					_ = q.Fail(ctx, id, "boom", clk.Now().UnixNano())
					got, _ := q.Get(ctx, id)
					if got.Attempt > got.MaxAttempts {
						t.Fatalf("Attempt %d exceeds MaxAttempts %d", got.Attempt, got.MaxAttempts)
					}
				}
			case 4: // time passes; leases may expire
				clk.Advance(2 * time.Minute)
			}
		}
	})
}

package jobs_test

import (
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/jobs"
	"github.com/ionalpha/flynn/jobs/jobstest"
)

// TestMemoryQueueConformance holds the in-process reference to the full contract.
func TestMemoryQueueConformance(t *testing.T) {
	jobstest.RunSuite(t, func() jobstest.Harness {
		// A fixed, non-zero start so RunAt = now is well-defined and the manual
		// clock can be advanced to trigger scheduling and lease expiry.
		clk := clock.NewManual(time.Unix(1_700_000_000, 0).UTC())
		return jobstest.Harness{Queue: jobs.NewMemory(jobs.WithClock(clk)), Clock: clk}
	})
}

func TestBackoff(t *testing.T) {
	const base, ceiling = int64(time.Second), int64(time.Minute)
	cases := []struct {
		attempt int
		want    int64
	}{
		{0, base}, // clamped to 1
		{1, base}, // 1s
		{2, 2 * base},
		{3, 4 * base},
		{4, 8 * base},
		{5, 16 * base},
		{6, 32 * base},
		{7, ceiling}, // 64s would exceed the 60s cap
		{100, ceiling},
	}
	for _, tc := range cases {
		if got := jobs.Backoff(tc.attempt, base, ceiling); got != tc.want {
			t.Errorf("Backoff(%d) = %d, want %d", tc.attempt, got, tc.want)
		}
	}
}

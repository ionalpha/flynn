package sqlite_test

import (
	"context"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
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

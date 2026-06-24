package jobs_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ionalpha/flynn/jobs"
)

// TestMemoryQueueConcurrentEnqueueClaim is a race regression: a job enqueued on
// one goroutine is claimed (and mutated in place) on another. Enqueue must return
// a value that does not alias the job it stores in the map, or copying that value
// out races with Claim's MarkClaimed. Runs clean only under -race with the fix.
func TestMemoryQueueConcurrentEnqueueClaim(t *testing.T) {
	q := jobs.NewMemory()
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if _, err := q.Enqueue(ctx, jobs.EnqueueParams{Queue: "w", Kind: "step"}); err != nil {
				t.Errorf("enqueue: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			if _, err := q.Claim(ctx, jobs.ClaimParams{Queue: "w", Limit: 4, LeaseFor: int64(time.Minute)}); err != nil {
				t.Errorf("claim: %v", err)
			}
		}()
	}
	wg.Wait()
}

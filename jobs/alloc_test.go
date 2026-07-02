//go:build !race

// Allocation ceilings for the claim path, in the style of chain/alloc_test.go:
// ceilings are the measured cost plus headroom; lower them when the cost
// drops, never raise them to absorb a regression. Excluded under -race
// (instrumentation skews allocation counts); dev/bench and the CI bench job
// run them. The claim-path guarantee under test: an idle claim costs the same
// with 100 terminal jobs retained as with 10,000, because terminal jobs leave
// the claim index when they settle.

package jobs_test

import (
	"testing"

	"github.com/ionalpha/flynn/jobs"
)

func claimAllocs(t *testing.T, terminal int) float64 {
	t.Helper()
	q, ctx := benchQueue(t, terminal)
	return testing.AllocsPerRun(100, func() {
		got, err := q.Claim(ctx, jobs.ClaimParams{Limit: 1, LeaseFor: 1})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Fatalf("claimed %d jobs from a drained queue", len(got))
		}
	})
}

func TestAllocCeilingClaimIdleConstantInTerminalBacklog(t *testing.T) {
	small := claimAllocs(t, 100)
	large := claimAllocs(t, 10_000)
	if small != large {
		t.Errorf("idle Claim allocates %.0f/op with 100 terminal jobs but %.0f/op with 10000: claim cost is scaling with the terminal backlog", small, large)
	}
	if large > 2 {
		t.Errorf("idle Claim allocates %.0f/op, over the 2 ceiling: a claim-path regression (or lower the ceiling if the cost legitimately grew)", large)
	}
}

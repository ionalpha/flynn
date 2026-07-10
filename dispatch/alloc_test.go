//go:build !race

package dispatch_test

import (
	"context"
	"testing"

	"github.com/ionalpha/flynn/dispatch"
)

// governAllocCeiling is what one governed action costs in allocations when no
// profile bundle is open, which is every production run. The waist runs on every
// tool call and every model call, so this number is a budget, not a curiosity.
//
// It was 10 before pprof labelling was added and it is 10 after: the labelling
// path is behind diag.Profiling(), so an unprofiled process never builds a label
// map and never wraps work in a closure. Lower it when the waist gets cheaper.
// Do not raise it without saying, here, what bought the allocation.
const governAllocCeiling = 10

// TestGovernAllocCeiling holds the disabled labelling path at zero cost. The
// build tag is not optional: AllocsPerRun counts the race detector's own
// bookkeeping, so this test is meaningless under -race and the bench job is where
// it runs.
func TestGovernAllocCeiling(t *testing.T) {
	d := dispatch.New()
	ctx := context.Background()
	a := dispatch.Action{Name: "tool:bash"}
	work := func(context.Context) (dispatch.Metering, error) { return dispatch.Metering{Tokens: 1}, nil }

	// Warm the counter cache and the sink before measuring, so the first call's
	// one-time costs are not charged to the steady state.
	if err := d.Govern(ctx, a, work); err != nil {
		t.Fatalf("Govern: %v", err)
	}
	got := testing.AllocsPerRun(200, func() { _ = d.Govern(ctx, a, work) })
	if got > governAllocCeiling {
		t.Errorf("Govern allocates %.0f objects per call, ceiling is %d", got, governAllocCeiling)
	}
}

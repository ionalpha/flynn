//go:build !race

// Allocation ceilings for the interpreter's per-element cost, in the style of
// chain/alloc_test.go: ceilings are the measured cost plus headroom; lower them
// when the cost drops, never raise them to absorb a regression. Excluded under
// -race (instrumentation skews allocation counts); dev/bench and the CI bench job
// run them. The guarantee under test: Decode compiles every expression and
// template once, so evaluating over N elements costs a few allocations per
// element for evaluation itself and none for parsing. Re-introducing a per-eval
// parse costs several allocations per element (lexed tokens plus AST nodes) and
// blows the ceiling.

package flow_test

import (
	"context"
	"testing"

	"github.com/ionalpha/flynn/internal/flow"
)

// mapAllocsPerElement runs a compiled transform map over n elements and reports
// allocations per element.
func mapAllocsPerElement(t *testing.T, n int) float64 {
	t.Helper()
	f, err := flow.Decode([]byte(`{"steps":[
		{"id":"m","op":"transform","transform":{"source":"config.items","map":"it + 1"}},
		{"op":"return","return":{"value":"{{steps.m}}"}}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	in := flow.New()
	items := make([]any, n)
	for i := range items {
		items[i] = float64(i)
	}
	config := map[string]any{"items": items}
	ctx := context.Background()
	total := testing.AllocsPerRun(20, func() {
		if _, err := in.Run(ctx, f, config); err != nil {
			t.Fatal(err)
		}
	})
	return total / float64(n)
}

func TestAllocCeilingTransformMapPerElement(t *testing.T) {
	small := mapAllocsPerElement(t, 100)
	large := mapAllocsPerElement(t, 1000)
	// Per-element cost must not grow with input size (parsing amortized to zero),
	// and must stay under a ceiling a per-eval parse cannot meet.
	if large > small+1 {
		t.Errorf("transform map allocates %.2f/element at 1000 elements but %.2f at 100: per-element cost is growing with input size", large, small)
	}
	if large > 6 {
		t.Errorf("transform map allocates %.2f/element, over the 6 ceiling: an eval-path regression, likely a re-introduced per-eval parse (or lower the ceiling if the cost legitimately dropped)", large)
	}
}

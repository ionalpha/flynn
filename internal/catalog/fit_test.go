package catalog

import (
	"testing"

	"pgregory.net/rapid"
)

func TestFeasibility(t *testing.T) {
	const gb = 1_000_000_000
	cases := []struct {
		name  string
		avail int64
		size  int64
		want  Fit
	}{
		{"comfortable", 24 * gb, 5 * gb, FitFeasible},
		{"exact headroom", 6 * gb, 5 * gb, FitFeasible}, // 5 + 20% = 6.0
		{"tight", 5*gb + 1, 5 * gb, FitTight},           // fits weights, not full headroom
		{"weights just fit", 5 * gb, 5 * gb, FitTight},  // avail == size
		{"over budget", 4 * gb, 5 * gb, FitOverBudget},  // weights do not fit
		{"unknown size", 8 * gb, 0, FitUnknown},
		{"unknown budget", 0, 5 * gb, FitUnknown},
	}
	for _, tc := range cases {
		if got := Feasibility(tc.avail, tc.size); got != tc.want {
			t.Errorf("%s: Feasibility(%d,%d)=%s, want %s", tc.name, tc.avail, tc.size, got, tc.want)
		}
	}
}

func TestFitForKindRules(t *testing.T) {
	api := ModelSpec{Kind: KindAPI}
	if api.FitFor(1) != FitFeasible {
		t.Fatal("an API model downloads nothing and should always be feasible")
	}
	noQuant := ModelSpec{Kind: KindLocal}
	if noQuant.FitFor(8_000_000_000) != FitUnknown {
		t.Fatal("a local model with no quant size is unknown")
	}
	local := ModelSpec{Kind: KindLocal, Quants: []Quant{{SizeBytes: 1_000_000_000, Format: FormatGGUF}}}
	if local.FitFor(24_000_000_000) != FitFeasible {
		t.Fatal("a 1GB model should fit a 24GB budget")
	}
}

// TestFeasibilityMonotonic is the property that makes a fit verdict trustworthy: more
// memory never makes a model fit worse. Encoded as an order feasible > tight >
// over-budget, raising the budget can only move a verdict up.
func TestFeasibilityMonotonic(t *testing.T) {
	rank := map[Fit]int{FitOverBudget: 0, FitTight: 1, FitFeasible: 2}
	rapid.Check(t, func(rt *rapid.T) {
		size := rapid.Int64Range(1, 1_000_000_000_000).Draw(rt, "size")
		a := rapid.Int64Range(1, 2_000_000_000_000).Draw(rt, "a")
		b := rapid.Int64Range(1, 2_000_000_000_000).Draw(rt, "b")
		if a > b {
			a, b = b, a
		}
		if rank[Feasibility(b, size)] < rank[Feasibility(a, size)] {
			rt.Fatalf("more memory fit worse: size=%d a=%d->%s b=%d->%s", size, a, Feasibility(a, size), b, Feasibility(b, size))
		}
	})
}

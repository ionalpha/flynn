package diag

import (
	"math"
	"testing"

	"pgregory.net/rapid"
)

// series generates a window of counter values. The alphabet is bounded so rapid
// spends its budget on the shapes the detector must separate (flat runs, dips,
// repeats, plateaus) rather than on arbitrary magnitudes, which the floors handle.
func series(minLen int) *rapid.Generator[[]float64] {
	return rapid.Custom(func(t *rapid.T) []float64 {
		n := rapid.IntRange(minLen, 24).Draw(t, "len")
		vs := make([]float64, n)
		for i := range vs {
			vs[i] = float64(rapid.IntRange(0, 40).Draw(t, "v"))
		}
		return vs
	})
}

// TestPropertyDetectNeverFiresWithoutGrowth. Whatever shape a window has, a
// counter that ends no higher than it started is not leaking. This is the property
// that keeps the detector honest against every series rapid can build, including
// the ones no reviewer would think to write down.
func TestPropertyDetectNeverFiresWithoutGrowth(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		vs := series(0).Draw(t, "vs")
		if len(vs) > 0 && vs[len(vs)-1] > vs[0] {
			t.Skip("this window grew")
		}
		th := Threshold{
			MinSlope: float64(rapid.IntRange(1, 10).Draw(t, "min_slope")),
			MinDelta: float64(rapid.IntRange(1, 10).Draw(t, "min_delta")),
		}
		if f, ok := detect(vs, th); ok {
			t.Fatalf("detect fired on a window that did not grow: %v (delta %v)", vs, f.Delta)
		}
	})
}

// TestPropertyDetectFiringImpliesEveryCondition. A firing is a conjunction, and a
// refactor that drops one conjunct must not be able to hide behind the others.
func TestPropertyDetectFiringImpliesEveryCondition(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		vs := series(4).Draw(t, "vs")
		th := Threshold{
			MinSlope: float64(rapid.IntRange(1, 5).Draw(t, "min_slope")),
			MinDelta: float64(rapid.IntRange(1, 20).Draw(t, "min_delta")),
		}
		f, ok := detect(vs, th)
		if !ok {
			return
		}
		if !staircase(vs) {
			t.Fatalf("detect fired on a window that is not a staircase: %v", vs)
		}
		if f.Slope < th.MinSlope {
			t.Fatalf("detect fired with slope %v below the floor %v: %v", f.Slope, th.MinSlope, vs)
		}
		if f.Delta < th.MinDelta {
			t.Fatalf("detect fired with delta %v below the floor %v: %v", f.Delta, th.MinDelta, vs)
		}
		if f.First != vs[0] || f.Last != vs[len(vs)-1] || f.Delta != f.Last-f.First {
			t.Fatalf("finding does not describe its window: %+v vs %v", f, vs)
		}
	})
}

// TestPropertyDetectIsTranslationInvariant. A counter's leak-ness is a property of
// its shape, not of where it sits. Ten thousand goroutines climbing by one is the
// same event as ten goroutines climbing by one, and a detector that disagreed would
// be measuring the process's size rather than its growth.
func TestPropertyDetectIsTranslationInvariant(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		vs := series(4).Draw(t, "vs")
		offset := float64(rapid.IntRange(-1000, 1_000_000).Draw(t, "offset"))
		th := Threshold{MinSlope: 1, MinDelta: 4}

		shifted := make([]float64, len(vs))
		for i, v := range vs {
			shifted[i] = v + offset
		}

		base, baseOK := detect(vs, th)
		got, gotOK := detect(shifted, th)
		if baseOK != gotOK {
			t.Fatalf("shifting %v by %v changed whether it fired (%v -> %v)", vs, offset, baseOK, gotOK)
		}
		if baseOK && (math.Abs(got.Slope-base.Slope) > 1e-6 || got.Delta != base.Delta) {
			t.Fatalf("shifting %v by %v changed slope %v -> %v or delta %v -> %v", vs, offset, base.Slope, got.Slope, base.Delta, got.Delta)
		}
	})
}

// risingSeries generates a strictly increasing window: a leak, in every shape a
// leak can take. Increments vary, so the fit is exercised over ragged growth and
// not only over a straight line.
func risingSeries() *rapid.Generator[[]float64] {
	return rapid.Custom(func(t *rapid.T) []float64 {
		n := rapid.IntRange(4, 24).Draw(t, "len")
		vs := make([]float64, n)
		vs[0] = float64(rapid.IntRange(0, 1000).Draw(t, "start"))
		for i := 1; i < n; i++ {
			vs[i] = vs[i-1] + float64(rapid.IntRange(1, 50).Draw(t, "step"))
		}
		return vs
	})
}

// TestPropertyEverySustainedRiseFires is the detector's other half: the shape tests
// prove it stays quiet, and this proves it does not stay quiet through a real leak.
// Any strictly increasing window clears the staircase rule, whatever its increments.
func TestPropertyEverySustainedRiseFires(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		vs := risingSeries().Draw(t, "vs")
		if _, ok := detect(vs, Threshold{MinSlope: 1, MinDelta: 1}); !ok {
			t.Fatalf("detect stayed quiet through a strictly rising window: %v", vs)
		}
	})
}

// TestPropertyDetectRejectsEveryReversal. Reversing a window that grew produces one
// that shrank, and a cache draining is never a leak. This catches a sign error that
// the unit tests' fixed shapes would miss on one branch.
func TestPropertyDetectRejectsEveryReversal(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		vs := risingSeries().Draw(t, "vs")
		th := Threshold{MinSlope: 1, MinDelta: 1}

		reversed := make([]float64, len(vs))
		for i, v := range vs {
			reversed[len(vs)-1-i] = v
		}
		if _, ok := detect(reversed, th); ok {
			t.Fatalf("detect fired on the reversal of a rising window: %v", reversed)
		}
	})
}

// TestPropertyAppendCappedIsABoundedWindow. The window rides a sampler that runs
// for the life of the process: it must hold the newest n values, in order, in
// bounded memory, for every sequence of pushes.
func TestPropertyAppendCappedIsABoundedWindow(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		n := rapid.IntRange(1, 16).Draw(t, "n")
		pushes := rapid.SliceOfN(rapid.IntRange(0, 99), 0, 64).Draw(t, "pushes")

		var got []int
		for _, v := range pushes {
			got = appendCapped(got, v, n)
			if len(got) > n {
				t.Fatalf("window holds %d values, cap is %d", len(got), n)
			}
			if cap(got) > n {
				t.Fatalf("window capacity %d exceeds %d: the sampler would grow without bound", cap(got), n)
			}
		}

		want := pushes
		if len(want) > n {
			want = want[len(want)-n:]
		}
		if len(got) != len(want) {
			t.Fatalf("window = %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("window = %v, want the newest %d in order: %v", got, n, want)
			}
		}
	})
}

package diag

// The detection rule itself, ahead of any watchdog. Whether anyone leaves --leak-watch
// on is decided here: every normal shape rises somewhere, and a naive "slope > 0"
// detector calls several of them leaks. Separation carries that, and the two floors
// reject growth too slow or too small to matter even when it is monotonic.

import "testing"

// ramp is a Threshold loose enough that any of the shapes below could clear the
// floors, so what the shape tests actually exercise is the separation rule rather
// than the floors. The floor tests set their own.
var ramp = Threshold{MinSlope: 0.5, MinDelta: 4}

// TestDetectFiresOnSustainedGrowth is the case the watchdog exists for: a counter
// that rises across the whole window and never once comes back.
func TestDetectFiresOnSustainedGrowth(t *testing.T) {
	vs := []float64{10, 12, 14, 16, 18, 20, 22, 24, 26, 28, 30, 32}

	f, ok := detect(vs, ramp)
	if !ok {
		t.Fatalf("detect(%v) did not fire on a clean ramp", vs)
	}
	if f.Slope != 2 {
		t.Errorf("slope = %v, want 2", f.Slope)
	}
	if f.Delta != 22 {
		t.Errorf("delta = %v, want 22", f.Delta)
	}
	if f.First != 10 || f.Last != 32 {
		t.Errorf("first,last = %v,%v, want 10,32", f.First, f.Last)
	}
}

// TestDetectDoesNotFireOnNormalShapes is the test that decides whether anyone
// leaves --leak-watch on. Every shape here rises somewhere, and a naive "slope > 0"
// detector calls several of them leaks. None of them is one.
func TestDetectDoesNotFireOnNormalShapes(t *testing.T) {
	cases := []struct {
		name string
		vs   []float64
		why  string
	}{
		{
			name: "flat",
			vs:   []float64{40, 40, 40, 40, 40, 40, 40, 40},
			why:  "a steady process",
		},
		{
			name: "sawtooth heap between collections",
			vs:   []float64{10, 30, 50, 12, 32, 52, 14, 34, 54, 16, 36, 56},
			why:  "the live heap rises and falls with the collector; the trend is up, the retention is not",
		},
		{
			name: "single spike",
			vs:   []float64{10, 10, 10, 10, 10, 90, 10, 10, 10, 10, 10, 10},
			why:  "one expensive turn, fully released",
		},
		{
			name: "step that settles",
			vs:   []float64{10, 10, 10, 10, 10, 10, 60, 60, 60, 60, 60, 60},
			why:  "a fan-out raised the floor once and held it; a step is not a slope",
		},
		{
			name: "fan-out that returns to baseline",
			vs:   []float64{20, 45, 70, 95, 120, 140, 120, 95, 70, 45, 22, 20},
			why:  "child agents spawned and were reaped",
		},
		{
			name: "noisy but level",
			vs:   []float64{50, 47, 53, 49, 51, 48, 52, 50, 47, 53, 49, 51},
			why:  "jitter without trend",
		},
		{
			name: "ramp with one dip",
			vs:   []float64{10, 12, 14, 16, 18, 20, 22, 11, 26, 28, 30, 32},
			why:  "growth that reverses even once is not yet sustained; the next window will catch it if it is real",
		},
		{
			name: "monotone decline",
			vs:   []float64{90, 80, 70, 60, 50, 40, 30, 20},
			why:  "a cache draining",
		},
		{
			name: "short window",
			vs:   []float64{1, 2, 3},
			why:  "three points admit any slope",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if f, ok := detect(tc.vs, ramp); ok {
				t.Errorf("detect fired on %v (slope %v, delta %v): %s", tc.vs, f.Slope, f.Delta, tc.why)
			}
		})
	}
}

// TestDetectHonoursBothFloors: separation alone is not enough. Growth too slow to
// matter over the life of a process, and growth too small to matter at all, are
// both rejected even though each rises monotonically.
func TestDetectHonoursBothFloors(t *testing.T) {
	// Rises by 1 per sample, cleanly separated: 11 over the window.
	slow := []float64{100, 101, 102, 103, 104, 105, 106, 107, 108, 109, 110, 111}

	if _, ok := detect(slow, Threshold{MinSlope: 10, MinDelta: 4}); ok {
		t.Error("detect fired on a slope of 1 against a floor of 10")
	}
	if _, ok := detect(slow, Threshold{MinSlope: 0.5, MinDelta: 500}); ok {
		t.Error("detect fired on a delta of 11 against a floor of 500")
	}
	if _, ok := detect(slow, Threshold{MinSlope: 0.5, MinDelta: 4}); !ok {
		t.Error("detect did not fire on a slope of 1 and a delta of 11 against floors it clears")
	}
}

// TestLeastSquaresSlope pins the fit itself, so a refactor of the one-pass form
// cannot quietly change what every threshold is measured against.
func TestLeastSquaresSlope(t *testing.T) {
	cases := []struct {
		vs   []float64
		want float64
	}{
		{[]float64{0, 1, 2, 3}, 1},
		{[]float64{0, 2, 4, 6}, 2},
		{[]float64{5, 5, 5, 5}, 0},
		{[]float64{3, 2, 1, 0}, -1},
		// A least-squares fit is not the endpoint difference: the dip pulls it down.
		{[]float64{0, 10, 0, 10}, 2},
	}
	for _, tc := range cases {
		if got := leastSquaresSlope(tc.vs); got != tc.want {
			t.Errorf("leastSquaresSlope(%v) = %v, want %v", tc.vs, got, tc.want)
		}
	}
	if got := leastSquaresSlope([]float64{7}); got != 0 {
		t.Errorf("leastSquaresSlope of one point = %v, want 0", got)
	}
}

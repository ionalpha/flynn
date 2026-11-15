package llm

import "math"

// Sampling pins the decoding parameters of a generation, so a run on weights you hold
// can be recorded and replayed. A hosted API cannot promise this.
type Sampling struct {
	// Seed fixes the random seed. A recorded run carries it, so a replay uses the same
	// value even where the runtime may not honor it.
	Seed int64
	// Temperature scales randomness. Zero is greedy decoding. A negative or non-finite
	// value normalizes to zero.
	Temperature float64
	// TopP is the nucleus-sampling cutoff in (0,1]. Zero keeps the server default; a value
	// outside [0,1] is clamped.
	TopP float64
}

// Deterministic reports whether output is guaranteed reproducible, which only greedy
// decoding is. A positive temperature also needs the runtime to honor the seed, and not
// every runtime does.
func (s Sampling) Deterministic() bool { return s.Temperature == 0 }

// Normalized clamps every field into its valid range and drops non-finite values, so a
// malformed sampling never reaches a backend.
func (s Sampling) Normalized() Sampling {
	if math.IsNaN(s.Temperature) || math.IsInf(s.Temperature, 0) || s.Temperature < 0 {
		s.Temperature = 0
	}
	switch {
	case math.IsNaN(s.TopP) || math.IsInf(s.TopP, 0) || s.TopP < 0:
		s.TopP = 0
	case s.TopP > 1:
		s.TopP = 1
	}
	return s
}

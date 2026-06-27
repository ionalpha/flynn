package llm

import "math"

// Sampling pins the decoding parameters of a generation so a local run can be reproduced. A
// hosted API cannot promise this, but a model whose weights you hold can: with a fixed seed
// and greedy or seeded decoding, the same request yields the same output. A Request leaves
// Sampling nil to take the server's defaults (free-running); setting it opts into pinned
// decoding, which is what makes a local run a recordable, repeatable artifact.
type Sampling struct {
	// Seed fixes the generation's random seed. With a positive temperature, reproducibility
	// depends on the runtime honoring the seed; a recorded run carries it regardless so a
	// later replay can use the same value.
	Seed int64
	// Temperature scales randomness. Zero is greedy decoding, which yields the same output
	// every time on the same weights and is the strongest reproducible mode; higher is more
	// random. A negative or non-finite value normalizes to zero.
	Temperature float64
	// TopP is the nucleus-sampling cutoff in (0,1]. Zero leaves the server default in place; a
	// value outside [0,1] normalizes into it.
	TopP float64
}

// Deterministic reports whether this sampling is in the guaranteed-reproducible regime. Greedy
// decoding (temperature zero) yields identical output on identical weights every time. With a
// positive temperature, reproducibility additionally needs the runtime to honor the seed, which
// is not guaranteed across runtimes, so that case is not reported as deterministic here.
func (s Sampling) Deterministic() bool { return s.Temperature == 0 }

// Normalized returns a copy with every field forced into its valid range: temperature floored
// at zero, top-p clamped to [0,1], and any non-finite value dropped, so a malformed sampling
// can never reach a backend as a garbage value.
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

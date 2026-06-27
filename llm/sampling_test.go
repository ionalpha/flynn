package llm

import (
	"math"
	"testing"

	"pgregory.net/rapid"
)

func TestSamplingDeterministic(t *testing.T) {
	if !(Sampling{Temperature: 0, Seed: 42}).Deterministic() {
		t.Fatal("greedy decoding (temperature 0) must be reported deterministic")
	}
	if (Sampling{Temperature: 0.7, Seed: 42}).Deterministic() {
		t.Fatal("a positive temperature is not guaranteed reproducible across runtimes")
	}
}

func TestSamplingNormalizedClamps(t *testing.T) {
	got := Sampling{Temperature: -2, TopP: 5, Seed: 7}.Normalized()
	if got.Temperature != 0 {
		t.Errorf("negative temperature = %v, want floored to 0", got.Temperature)
	}
	if got.TopP != 1 {
		t.Errorf("top-p above 1 = %v, want clamped to 1", got.TopP)
	}
	if got.Seed != 7 {
		t.Errorf("seed = %d, want preserved 7", got.Seed)
	}

	nan := Sampling{Temperature: math.NaN(), TopP: math.Inf(1)}.Normalized()
	if nan.Temperature != 0 || nan.TopP != 0 {
		t.Fatalf("non-finite values must drop to 0, got %+v", nan)
	}
}

// TestSamplingNormalizedInvariants asserts Normalized maps any input, including hostile
// values, onto a valid sampling, and that applying it twice changes nothing.
func TestSamplingNormalizedInvariants(t *testing.T) {
	floats := []float64{-5, -0.1, 0, 0.5, 1, 2, 100, math.NaN(), math.Inf(1), math.Inf(-1)}
	rapid.Check(t, func(rt *rapid.T) {
		s := Sampling{
			Seed:        rapid.Int64().Draw(rt, "seed"),
			Temperature: rapid.SampledFrom(floats).Draw(rt, "temp"),
			TopP:        rapid.SampledFrom(floats).Draw(rt, "topp"),
		}
		n := s.Normalized()
		if math.IsNaN(n.Temperature) || math.IsInf(n.Temperature, 0) || n.Temperature < 0 {
			rt.Fatalf("temperature not finite and non-negative: %v", n.Temperature)
		}
		if math.IsNaN(n.TopP) || math.IsInf(n.TopP, 0) || n.TopP < 0 || n.TopP > 1 {
			rt.Fatalf("top-p out of [0,1]: %v", n.TopP)
		}
		if n.Seed != s.Seed {
			rt.Fatalf("seed must be preserved: got %d, want %d", n.Seed, s.Seed)
		}
		if n.Normalized() != n {
			rt.Fatalf("Normalized is not idempotent: %+v", n)
		}
	})
}

// FuzzSamplingNormalized throws arbitrary parameters at Normalized and asserts it never panics
// and always yields a valid sampling.
func FuzzSamplingNormalized(f *testing.F) {
	f.Add(int64(0), 0.0, 0.0)
	f.Add(int64(42), -1.0, 2.0)
	f.Add(int64(-9), math.NaN(), math.Inf(1))

	f.Fuzz(func(t *testing.T, seed int64, temp, topP float64) {
		n := Sampling{Seed: seed, Temperature: temp, TopP: topP}.Normalized()
		if math.IsNaN(n.Temperature) || math.IsInf(n.Temperature, 0) || n.Temperature < 0 {
			t.Fatalf("temperature invalid: %v", n.Temperature)
		}
		if math.IsNaN(n.TopP) || math.IsInf(n.TopP, 0) || n.TopP < 0 || n.TopP > 1 {
			t.Fatalf("top-p invalid: %v", n.TopP)
		}
	})
}

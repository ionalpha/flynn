package serve

import (
	"math"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// assertStatsInvariants checks the universal RuntimeStats invariants: the gauge is a finite
// fraction in [0,1], counts are non-negative, and throughputs are finite and non-negative.
// Any reading the control plane sees, from any source, must satisfy these.
func assertStatsInvariants(rt *rapid.T, s RuntimeStats) {
	rt.Helper()
	if math.IsNaN(s.KVCacheUsage) || math.IsInf(s.KVCacheUsage, 0) || s.KVCacheUsage < 0 || s.KVCacheUsage > 1 {
		rt.Fatalf("KVCacheUsage out of [0,1]: %v", s.KVCacheUsage)
	}
	if s.RequestsRunning < 0 || s.RequestsWaiting < 0 {
		rt.Fatalf("negative request counts: running=%d waiting=%d", s.RequestsRunning, s.RequestsWaiting)
	}
	for _, v := range []float64{s.DecodeTokensPerSec, s.PromptTokensPerSec} {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			rt.Fatalf("throughput not finite and non-negative: %v", v)
		}
	}
}

// TestToStatsInvariants feeds arbitrary, often hostile, values for each known metric (some
// omitted) through the mapping and asserts the result always satisfies the invariants and
// never panics. A reachable runtime can report anything; the snapshot must stay sane.
func TestToStatsInvariants(t *testing.T) {
	tokens := []string{"0", "0.5", "1", "0.999", "-3", "5", "1e9", "1e-9", "NaN", "Inf", "+Inf", "-Inf", "abc", "", "   "}
	names := []string{
		llamaCppMetricNames.kvCacheUsage,
		llamaCppMetricNames.requestsRunning,
		llamaCppMetricNames.requestsWaiting,
		llamaCppMetricNames.decodeTokPerSec,
		llamaCppMetricNames.promptTokPerSec,
	}
	rapid.Check(t, func(rt *rapid.T) {
		var b strings.Builder
		for _, name := range names {
			if rapid.Bool().Draw(rt, "omit") {
				continue
			}
			tok := rapid.SampledFrom(tokens).Draw(rt, "val")
			b.WriteString(name)
			b.WriteByte(' ')
			b.WriteString(tok)
			b.WriteByte('\n')
		}
		assertStatsInvariants(rt, llamaCppMetricNames.toStats([]byte(b.String())))
	})
}

// TestNormalizedAlwaysValidAndIdempotent asserts normalized maps any struct, including ones
// with out-of-range and non-finite fields, onto a valid snapshot, and that applying it
// twice changes nothing.
func TestNormalizedAlwaysValidAndIdempotent(t *testing.T) {
	floats := []float64{-5, -0.1, 0, 0.5, 1, 2, 100, 1e18, math.NaN(), math.Inf(1), math.Inf(-1)}
	rapid.Check(t, func(rt *rapid.T) {
		s := RuntimeStats{
			Known:              rapid.Bool().Draw(rt, "known"),
			KVCacheUsage:       rapid.SampledFrom(floats).Draw(rt, "kv"),
			RequestsRunning:    rapid.IntRange(-100, 100).Draw(rt, "running"),
			RequestsWaiting:    rapid.IntRange(-100, 100).Draw(rt, "waiting"),
			DecodeTokensPerSec: rapid.SampledFrom(floats).Draw(rt, "decode"),
			PromptTokensPerSec: rapid.SampledFrom(floats).Draw(rt, "prompt"),
		}
		n := s.normalized()
		assertStatsInvariants(rt, n)
		if n.normalized() != n {
			rt.Fatalf("normalized is not idempotent: %+v", n)
		}
	})
}

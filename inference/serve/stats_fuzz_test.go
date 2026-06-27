package serve

import (
	"math"
	"testing"
)

// FuzzParseProm throws arbitrary bytes at the metrics parser and the mapping, asserting
// neither panics and that no parsed value is non-finite and no mapped snapshot escapes its
// invariants. The metrics endpoint is untrusted input to the control plane, so a hostile
// or truncated payload must degrade to missing values, never crash or poison a reading.
func FuzzParseProm(f *testing.F) {
	f.Add([]byte("llamacpp:kv_cache_usage_ratio 0.5\n"))
	f.Add([]byte("# help\nfoo{a=\"b\"} 1\nbar NaN\nbaz +Inf\n"))
	f.Add([]byte("llamacpp:requests_processing 3\nllamacpp:requests_deferred 2\n"))
	f.Add([]byte("{}{}{} \x00\n\n   \n}{"))
	f.Add([]byte("vllm:gpu_cache_usage_perc 1e999\n"))
	f.Add([]byte(""))

	f.Fuzz(func(t *testing.T, data []byte) {
		for name, v := range parseProm(data) {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				t.Fatalf("parseProm returned a non-finite value for %q: %v", name, v)
			}
		}
		for _, n := range []promNames{llamaCppMetricNames, vllmMetricNames} {
			s := n.toStats(data)
			if math.IsNaN(s.KVCacheUsage) || math.IsInf(s.KVCacheUsage, 0) || s.KVCacheUsage < 0 || s.KVCacheUsage > 1 {
				t.Fatalf("KVCacheUsage out of [0,1]: %v", s.KVCacheUsage)
			}
			if s.RequestsRunning < 0 || s.RequestsWaiting < 0 {
				t.Fatalf("negative counts: %+v", s)
			}
			if s.DecodeTokensPerSec < 0 || s.PromptTokensPerSec < 0 {
				t.Fatalf("negative throughput: %+v", s)
			}
		}
	})
}

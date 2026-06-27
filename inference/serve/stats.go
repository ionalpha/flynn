package serve

import (
	"context"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/observe"
)

// RuntimeStats is a point-in-time, best-effort reading of how loaded a running model
// server is: how full its key/value cache is and how many requests it is working on. It
// is the control-plane view the orchestrator and router read to make eviction and routing
// decisions, kept distinct from the metrics pushed to a backend through observe.Meter.
//
// Fields are best-effort. A runtime that does not report a value leaves it at its zero,
// and Known reports whether any usable reading was obtained at all: a false Known means
// the snapshot carries no information (the runtime was unreachable, hung, or spoke an
// unparseable dialect) and a reader must treat the load as unknown rather than as zero.
type RuntimeStats struct {
	// Known is true when a usable reading was obtained and parsed. A false Known means the
	// snapshot carries no information and the load must be treated as unknown, not as zero.
	Known bool
	// KVCacheUsage is the fraction of the key/value cache in use, in [0,1]. It is the
	// primary memory-pressure signal an eviction policy reads.
	KVCacheUsage float64
	// RequestsRunning is the number of requests the server is actively decoding.
	RequestsRunning int
	// RequestsWaiting is the number of requests queued behind the running ones.
	RequestsWaiting int
	// DecodeTokensPerSec is the recent generation throughput, in tokens per second.
	DecodeTokensPerSec float64
	// PromptTokensPerSec is the recent prefill throughput, in tokens per second.
	PromptTokensPerSec float64
	// CollectedAt is when the reading was taken, stamped from the manager clock so it is
	// deterministic in tests and consistent across the process.
	CollectedAt time.Time
}

// normalized returns a copy with every field forced into its valid range: the gauge
// clamped to [0,1], counts and throughputs floored at zero, and any non-finite value
// dropped to zero. It is applied to whatever a source returns, so a buggy or hostile
// source can never hand the control plane a garbage reading.
func (s RuntimeStats) normalized() RuntimeStats {
	s.KVCacheUsage = clampUnit(s.KVCacheUsage)
	s.DecodeTokensPerSec = clampNonNeg(s.DecodeTokensPerSec)
	s.PromptTokensPerSec = clampNonNeg(s.PromptTokensPerSec)
	if s.RequestsRunning < 0 {
		s.RequestsRunning = 0
	}
	if s.RequestsWaiting < 0 {
		s.RequestsWaiting = 0
	}
	return s
}

// StatsSource reads a load snapshot from a runtime serving at a loopback base URL. There
// is one implementation per runtime dialect, because the metric names differ. It returns
// a snapshot with Known false (and a nil error) when the runtime is reachable but reports
// nothing usable, and an error only when the read itself fails, which the manager turns
// into an unknown reading.
type StatsSource interface {
	Stats(ctx context.Context, baseURL string) (RuntimeStats, error)
}

// NopStatsSource reports every runtime as unknown load. It is the default for a runtime
// with no registered source, so the standalone path stays zero-setup.
type NopStatsSource struct{}

// Stats implements StatsSource by always reporting an unknown load.
func (NopStatsSource) Stats(context.Context, string) (RuntimeStats, error) {
	return RuntimeStats{}, nil
}

var _ StatsSource = NopStatsSource{}

// WithStatsSource registers a load reader for servers of a given runtime, keyed by the
// runtime name recorded on each server (for example "llama.cpp"). Without one for a
// runtime, Stats reports the load as unknown rather than failing.
func WithStatsSource(runtime string, src StatsSource) Option {
	return func(m *Manager) {
		if runtime != "" && src != nil {
			m.stats[runtime] = src
		}
	}
}

// Stats reads a best-effort load snapshot for a running model. It returns an error only
// when there is no recorded server for the model. A server that is present but cannot be
// read (no source for its runtime, an unreachable or hung endpoint, an unparseable reply)
// yields a snapshot with Known false, never an error, so the control plane can always read
// the load without a failing call breaking it. The reading is normalized and stamped with
// the manager clock.
func (m *Manager) Stats(ctx context.Context, modelID string) (RuntimeStats, error) {
	rec, ok, err := m.reg.Get(modelID)
	if err != nil {
		return RuntimeStats{}, err
	}
	if !ok {
		return RuntimeStats{}, fault.New(fault.Terminal, "serve_stats_no_server",
			"serve: no running server for model "+modelID)
	}
	now := m.clk.Now()
	src := m.stats[rec.Runtime]
	if src == nil {
		return RuntimeStats{CollectedAt: now}, nil
	}
	s, err := src.Stats(ctx, rec.BaseURL)
	if err != nil {
		// A read failure means the load is unknown right now, not that the call failed: a
		// hung or flapping runtime must not break a control-plane read.
		return RuntimeStats{CollectedAt: now}, nil //nolint:nilerr // a failed read is reported as unknown load, not surfaced as a call error
	}
	s = s.normalized()
	s.CollectedAt = now
	return s, nil
}

// EmitStats records a load snapshot on the meter so a host's telemetry backend sees the
// same numbers the control plane reads. The Meter port offers counters and histograms but
// no gauge instrument, so the snapshot's gauge-like values are recorded as histogram
// observations, which a backend renders as the per-model distribution and last value. A
// snapshot with Known false, or a nil meter, records nothing.
func EmitStats(ctx context.Context, m observe.Meter, modelID string, s RuntimeStats) {
	if m == nil || !s.Known {
		return
	}
	model := observe.String("model", modelID)
	m.Histogram("local.kv_cache_usage").Record(ctx, s.KVCacheUsage, model)
	m.Histogram("local.requests_running").Record(ctx, float64(s.RequestsRunning), model)
	m.Histogram("local.requests_waiting").Record(ctx, float64(s.RequestsWaiting), model)
	m.Histogram("local.decode_tokens_per_sec").Record(ctx, s.DecodeTokensPerSec, model)
	m.Histogram("local.prompt_tokens_per_sec").Record(ctx, s.PromptTokensPerSec, model)
}

// promNames maps the RuntimeStats fields onto a runtime's Prometheus metric names. The two
// dialects Flynn serves are filled in by llamaCppMetricNames and vllmMetricNames.
type promNames struct {
	kvCacheUsage    string
	requestsRunning string
	requestsWaiting string
	decodeTokPerSec string
	promptTokPerSec string
}

var llamaCppMetricNames = promNames{
	kvCacheUsage:    "llamacpp:kv_cache_usage_ratio",
	requestsRunning: "llamacpp:requests_processing",
	requestsWaiting: "llamacpp:requests_deferred",
	decodeTokPerSec: "llamacpp:predicted_tokens_seconds",
	promptTokPerSec: "llamacpp:prompt_tokens_seconds",
}

// vllmMetricNames maps onto vLLM's process-wide gauge metrics, read the same way as the
// llama.cpp set.
var vllmMetricNames = promNames{
	kvCacheUsage:    "vllm:gpu_cache_usage_perc",
	requestsRunning: "vllm:num_requests_running",
	requestsWaiting: "vllm:num_requests_waiting",
	decodeTokPerSec: "vllm:avg_generation_throughput_toks_per_s",
	promptTokPerSec: "vllm:avg_prompt_throughput_toks_per_s",
}

// toStats maps a parsed metric payload onto a normalized RuntimeStats. Known is true when
// at least one mapped metric was present, so a reachable runtime that reported nothing we
// understand is reported as unknown rather than as an all-zero load.
func (n promNames) toStats(data []byte) RuntimeStats {
	vals := parseProm(data)
	var s RuntimeStats
	if v, ok := vals[n.kvCacheUsage]; ok {
		s.KVCacheUsage = v
		s.Known = true
	}
	if v, ok := vals[n.requestsRunning]; ok {
		s.RequestsRunning = countFrom(v)
		s.Known = true
	}
	if v, ok := vals[n.requestsWaiting]; ok {
		s.RequestsWaiting = countFrom(v)
		s.Known = true
	}
	if v, ok := vals[n.decodeTokPerSec]; ok {
		s.DecodeTokensPerSec = v
		s.Known = true
	}
	if v, ok := vals[n.promptTokPerSec]; ok {
		s.PromptTokensPerSec = v
		s.Known = true
	}
	return s.normalized()
}

// parseProm scans Prometheus text exposition format and returns the first value seen for
// each metric name, with labels and timestamps ignored. It is deliberately forgiving:
// comment and blank lines are skipped, a line that does not parse as a sample is ignored,
// and a non-finite value is dropped, so a truncated, reordered, or hostile payload yields
// missing values rather than a panic or a poisoned reading. The runtime metrics read here
// are process-wide single-series gauges and counters, so dropping labels is safe.
func parseProm(data []byte) map[string]float64 {
	out := map[string]float64{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' {
			continue
		}
		name, rest := splitSample(line)
		if name == "" || rest == "" {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		v, err := strconv.ParseFloat(fields[0], 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			continue
		}
		if _, seen := out[name]; !seen {
			out[name] = v
		}
	}
	return out
}

// splitSample splits a Prometheus sample line into its metric name and the remainder that
// begins with the value. A labelled sample (name{...}) has its label block skipped to the
// last closing brace; an unlabelled one splits at the first whitespace. It never indexes
// out of range, so any malformed line returns empty and is skipped by the caller.
func splitSample(line string) (name, rest string) {
	if i := strings.IndexByte(line, '{'); i >= 0 {
		j := strings.LastIndexByte(line, '}')
		if j < i {
			return "", ""
		}
		return strings.TrimSpace(line[:i]), strings.TrimSpace(line[j+1:])
	}
	i := strings.IndexAny(line, " \t")
	if i < 0 {
		return "", ""
	}
	return line[:i], strings.TrimSpace(line[i+1:])
}

// metricsURL derives a runtime's Prometheus endpoint from its OpenAI-compatible base URL.
// The model API is served under a /v1 path while metrics are exposed at /metrics on the
// server root, so the version suffix is trimmed before the metrics path is appended.
func metricsURL(baseURL string) string {
	u := strings.TrimRight(baseURL, "/")
	u = strings.TrimSuffix(u, "/v1")
	u = strings.TrimRight(u, "/")
	return u + "/metrics"
}

// clampUnit forces v into [0,1], mapping a non-finite or negative value to 0.
func clampUnit(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// clampNonNeg floors v at 0, mapping a non-finite value to 0.
func clampNonNeg(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0
	}
	return v
}

// countFrom converts a metric value to a non-negative count, dropping a non-finite or
// negative value to 0.
func countFrom(v float64) int {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0
	}
	return int(v)
}

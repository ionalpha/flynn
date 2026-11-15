package serve

import (
	"bytes"
	"context"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/observe"
)

// RuntimeStats is a best-effort reading of how loaded a running model server is, for
// the orchestrator's eviction and the router's routing. Metrics for a backend go
// through observe.Meter instead. A value the runtime does not report stays zero.
type RuntimeStats struct {
	// Known is false when the snapshot carries no information (unreachable, hung or an
	// unparseable dialect), so a reader treats the load as unknown, never as zero.
	Known bool
	// KVCacheUsage is the fraction of the key/value cache in use, in [0,1], and the
	// memory-pressure reading an eviction policy uses.
	KVCacheUsage float64
	// RequestsRunning is the number of requests being decoded.
	RequestsRunning int
	// RequestsWaiting is the number of requests queued behind the running ones.
	RequestsWaiting int
	// DecodeTokensPerSec is recent generation throughput, in tokens per second.
	DecodeTokensPerSec float64
	// PromptTokensPerSec is recent prefill throughput, in tokens per second.
	PromptTokensPerSec float64
	// CollectedAt is stamped from the manager clock, so it is deterministic in tests.
	CollectedAt time.Time
}

// normalized clamps the gauge to [0,1], floors counts and throughputs at zero and drops
// non-finite values, so a buggy or hostile source cannot hand the control plane garbage.
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

// StatsSource reads a load snapshot from a runtime at a loopback base URL, one
// implementation per metric dialect. A reachable runtime that reports nothing usable
// gives Known false and a nil error; an error means the read itself failed.
type StatsSource interface {
	Stats(ctx context.Context, baseURL string) (RuntimeStats, error)
}

// NopStatsSource reports every load as unknown. It is the default for a runtime with no
// registered source, so the standalone path needs no setup.
type NopStatsSource struct{}

// Stats always reports an unknown load.
func (NopStatsSource) Stats(context.Context, string) (RuntimeStats, error) {
	return RuntimeStats{}, nil
}

var _ StatsSource = NopStatsSource{}

// WithStatsSource registers a load reader for servers of a runtime, keyed by the runtime
// name recorded on each server (for example "llama.cpp").
func WithStatsSource(runtime string, src StatsSource) Option {
	return func(m *Manager) {
		if runtime != "" && src != nil {
			m.stats[runtime] = src
		}
	}
}

// Stats reads a normalized load snapshot for a running model, stamped with the manager
// clock. It errors only when no server is recorded for the model. An unreadable server
// yields Known false, so a hung runtime cannot break a control-plane read.
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
		// A failed read is unknown load, not a failed call.
		return RuntimeStats{CollectedAt: now}, nil //nolint:nilerr // a failed read is reported as unknown load, not surfaced as a call error
	}
	s = s.normalized()
	s.CollectedAt = now
	return s, nil
}

// EmitStats records a load snapshot on the meter, so a telemetry backend sees the
// numbers the control plane reads. The Meter port has no gauge, so gauge values are
// recorded as histogram observations. Known false or a nil meter records nothing.
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

// promNames maps the RuntimeStats fields onto one runtime's Prometheus metric names.
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

var vllmMetricNames = promNames{
	kvCacheUsage:    "vllm:gpu_cache_usage_perc",
	requestsRunning: "vllm:num_requests_running",
	requestsWaiting: "vllm:num_requests_waiting",
	decodeTokPerSec: "vllm:avg_generation_throughput_toks_per_s",
	promptTokPerSec: "vllm:avg_prompt_throughput_toks_per_s",
}

// toStats sets Known when at least one mapped metric was present, so a runtime that
// reported nothing we understand reads as unknown rather than as zero load.
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

// parseProm returns the first value of each metric name in Prometheus text format,
// ignoring labels and timestamps. Unparseable lines and non-finite values are skipped,
// so a truncated or hostile payload yields missing values, never a panic. The metrics
// read here are single-series, so dropping labels is safe.
func parseProm(data []byte) map[string]float64 {
	out := map[string]float64{}
	// Scan the raw bytes line by line, because splitting the whole payload allocates a
	// copy of every series when only a handful are read.
	for len(data) > 0 {
		var raw []byte
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			raw, data = data[:i], data[i+1:]
		} else {
			raw, data = data, nil
		}
		line := strings.TrimSpace(string(raw))
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

// splitSample splits a sample line into its metric name and the rest, which begins with
// the value. A label block is skipped to its last closing brace. It never indexes out of
// range, so a malformed line returns empty and is skipped.
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

// metricsURL trims the /v1 suffix from the OpenAI-compatible base URL, because metrics
// are served at /metrics on the server root.
func metricsURL(baseURL string) string {
	u := strings.TrimRight(baseURL, "/")
	u = strings.TrimSuffix(u, "/v1")
	u = strings.TrimRight(u, "/")
	return u + "/metrics"
}

// clampUnit forces v into [0,1], mapping a non-finite value to 0.
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

// countFrom converts v to a non-negative count, mapping a non-finite value to 0.
func countFrom(v float64) int {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0
	}
	return int(v)
}

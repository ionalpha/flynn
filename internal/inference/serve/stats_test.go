package serve

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/observe"
)

const sampleLlamaMetrics = `# HELP llamacpp:kv_cache_usage_ratio KV-cache usage ratio
# TYPE llamacpp:kv_cache_usage_ratio gauge
llamacpp:kv_cache_usage_ratio 0.42
llamacpp:requests_processing 3
llamacpp:requests_deferred 2
llamacpp:predicted_tokens_seconds 61.5
llamacpp:prompt_tokens_seconds 540.2
`

func TestLlamaCppToStatsMapsKnownMetrics(t *testing.T) {
	s := llamaCppMetricNames.toStats([]byte(sampleLlamaMetrics))
	if !s.Known {
		t.Fatal("a payload with mapped metrics must be Known")
	}
	if s.KVCacheUsage != 0.42 {
		t.Errorf("KVCacheUsage = %v, want 0.42", s.KVCacheUsage)
	}
	if s.RequestsRunning != 3 || s.RequestsWaiting != 2 {
		t.Errorf("requests = (%d,%d), want (3,2)", s.RequestsRunning, s.RequestsWaiting)
	}
	if s.DecodeTokensPerSec != 61.5 || s.PromptTokensPerSec != 540.2 {
		t.Errorf("throughput = (%v,%v), want (61.5,540.2)", s.DecodeTokensPerSec, s.PromptTokensPerSec)
	}
}

func TestToStatsUnknownWhenNoMappedMetricPresent(t *testing.T) {
	// vLLM metric names do not appear in a llama.cpp payload, so the result is unknown
	// rather than an all-zero (and misleading) load.
	s := vllmMetricNames.toStats([]byte(sampleLlamaMetrics))
	if s.Known {
		t.Fatalf("expected unknown load, got %+v", s)
	}
}

func TestToStatsClampsOutOfRangeValues(t *testing.T) {
	in := "llamacpp:kv_cache_usage_ratio 5\n" +
		"llamacpp:requests_processing -4\n" +
		"llamacpp:predicted_tokens_seconds -1\n"
	s := llamaCppMetricNames.toStats([]byte(in))
	if s.KVCacheUsage != 1 {
		t.Errorf("KVCacheUsage = %v, want clamped to 1", s.KVCacheUsage)
	}
	if s.RequestsRunning != 0 {
		t.Errorf("RequestsRunning = %d, want floored to 0", s.RequestsRunning)
	}
	if s.DecodeTokensPerSec != 0 {
		t.Errorf("DecodeTokensPerSec = %v, want floored to 0", s.DecodeTokensPerSec)
	}
}

func TestParsePromIgnoresGarbageLabelsAndNonFinite(t *testing.T) {
	in := "# a comment\n" +
		"foo{a=\"b\",c=\"d\"} 1.5\n" +
		"bar 2\n" +
		"broken line here\n" +
		"baz NaN\n" +
		"qux +Inf\n" +
		"foo 9\n"
	v := parseProm([]byte(in))
	if v["foo"] != 1.5 {
		t.Errorf("foo = %v, want first value 1.5 (labels stripped, first wins)", v["foo"])
	}
	if v["bar"] != 2 {
		t.Errorf("bar = %v, want 2", v["bar"])
	}
	if _, ok := v["baz"]; ok {
		t.Error("a NaN value must be dropped")
	}
	if _, ok := v["qux"]; ok {
		t.Error("an Inf value must be dropped")
	}
	if _, ok := v["broken"]; ok {
		t.Error("a non-numeric value must be dropped")
	}
}

func TestMetricsURL(t *testing.T) {
	cases := map[string]string{
		"http://127.0.0.1:8080/v1":  "http://127.0.0.1:8080/metrics",
		"http://127.0.0.1:8080/v1/": "http://127.0.0.1:8080/metrics",
		"http://127.0.0.1:8080":     "http://127.0.0.1:8080/metrics",
		"http://host/v1":            "http://host/metrics",
	}
	for in, want := range cases {
		if got := metricsURL(in); got != want {
			t.Errorf("metricsURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// fakeStatsSource returns a scripted snapshot or error.
type fakeStatsSource struct {
	s   RuntimeStats
	err error
}

func (f fakeStatsSource) Stats(context.Context, string) (RuntimeStats, error) {
	return f.s, f.err
}

func testManagerWithStats(t *testing.T, runtime string, src StatsSource) (*Manager, *Registry) {
	t.Helper()
	reg := NewRegistry(t.TempDir())
	m := NewManager(&fakeLauncher{proc: newFakeProc(1)}, alwaysHealthy, OSKiller, reg,
		withClock(clock.NewManual(time.Unix(1000, 0))),
		WithStatsSource(runtime, src))
	return m, reg
}

func TestManagerStatsNoServerErrors(t *testing.T) {
	m, _ := testManager(t, &fakeLauncher{proc: newFakeProc(1)}, alwaysHealthy, OSKiller)
	if _, err := m.Stats(context.Background(), "absent"); err == nil {
		t.Fatal("Stats for a model with no recorded server must error")
	}
}

func TestManagerStatsUnknownWithoutSource(t *testing.T) {
	m, reg := testManager(t, &fakeLauncher{proc: newFakeProc(1)}, alwaysHealthy, OSKiller)
	_ = reg.Put(Record{ModelID: "m", Runtime: "llama.cpp", BaseURL: "http://127.0.0.1:1/v1"})
	s, err := m.Stats(context.Background(), "m")
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if s.Known {
		t.Fatal("a runtime with no registered source must report unknown load")
	}
	if s.CollectedAt.Unix() != 1000 {
		t.Fatalf("CollectedAt = %d, want stamped from the manual clock (1000)", s.CollectedAt.Unix())
	}
}

func TestManagerStatsReadsSourceAndStamps(t *testing.T) {
	src := fakeStatsSource{s: RuntimeStats{Known: true, KVCacheUsage: 0.5, RequestsRunning: 2}}
	m, reg := testManagerWithStats(t, "llama.cpp", src)
	_ = reg.Put(Record{ModelID: "m", Runtime: "llama.cpp", BaseURL: "http://127.0.0.1:1/v1"})
	s, err := m.Stats(context.Background(), "m")
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if !s.Known || s.KVCacheUsage != 0.5 || s.RequestsRunning != 2 {
		t.Fatalf("snapshot = %+v, want the source reading", s)
	}
	if s.CollectedAt.Unix() != 1000 {
		t.Fatalf("CollectedAt = %d, want 1000", s.CollectedAt.Unix())
	}
}

func TestManagerStatsDegradesOnSourceError(t *testing.T) {
	src := fakeStatsSource{err: errors.New("connection refused")}
	m, reg := testManagerWithStats(t, "llama.cpp", src)
	_ = reg.Put(Record{ModelID: "m", Runtime: "llama.cpp", BaseURL: "http://127.0.0.1:1/v1"})
	s, err := m.Stats(context.Background(), "m")
	if err != nil {
		t.Fatalf("a source read failure must not surface as an error, got %v", err)
	}
	if s.Known {
		t.Fatal("a failed read must report unknown load")
	}
}

func TestManagerStatsNormalizesGarbageFromSource(t *testing.T) {
	// Even a misbehaving source that hands back impossible values must not reach the
	// control plane unnormalized.
	src := fakeStatsSource{s: RuntimeStats{Known: true, KVCacheUsage: 5, RequestsRunning: -3, DecodeTokensPerSec: math.Inf(1)}}
	m, reg := testManagerWithStats(t, "llama.cpp", src)
	_ = reg.Put(Record{ModelID: "m", Runtime: "llama.cpp", BaseURL: "http://127.0.0.1:1/v1"})
	s, _ := m.Stats(context.Background(), "m")
	if s.KVCacheUsage != 1 || s.RequestsRunning != 0 || s.DecodeTokensPerSec != 0 {
		t.Fatalf("snapshot not normalized: %+v", s)
	}
}

// fakeMeter records the values handed to its histograms, so a test can assert what
// EmitStats reported.
type fakeMeter struct {
	hist map[string][]float64
}

func newFakeMeter() *fakeMeter { return &fakeMeter{hist: map[string][]float64{}} }

func (f *fakeMeter) Counter(string) observe.Counter          { return nopCounterRec{} }
func (f *fakeMeter) Histogram(name string) observe.Histogram { return fakeHist{m: f, name: name} }

type nopCounterRec struct{}

func (nopCounterRec) Add(context.Context, int64, ...observe.Field) {}

type fakeHist struct {
	m    *fakeMeter
	name string
}

func (h fakeHist) Record(_ context.Context, v float64, _ ...observe.Field) {
	h.m.hist[h.name] = append(h.m.hist[h.name], v)
}

func TestEmitStatsRecordsKnownSnapshot(t *testing.T) {
	m := newFakeMeter()
	EmitStats(context.Background(), m, "model-x", RuntimeStats{Known: true, KVCacheUsage: 0.3, RequestsRunning: 1})
	if got := m.hist["local.kv_cache_usage"]; len(got) != 1 || got[0] != 0.3 {
		t.Fatalf("kv_cache_usage records = %v, want [0.3]", got)
	}
	if len(m.hist["local.requests_running"]) != 1 {
		t.Fatalf("requests_running not recorded: %v", m.hist)
	}
}

func TestEmitStatsSkipsUnknown(t *testing.T) {
	m := newFakeMeter()
	EmitStats(context.Background(), m, "x", RuntimeStats{Known: false, KVCacheUsage: 0.9})
	if len(m.hist) != 0 {
		t.Fatalf("an unknown snapshot must record nothing, recorded %v", m.hist)
	}
}

func TestEmitStatsNilMeterIsSafe(_ *testing.T) {
	// A nil meter must be a no-op rather than a panic; reaching the next line is the assert.
	EmitStats(context.Background(), nil, "x", RuntimeStats{Known: true, KVCacheUsage: 0.5})
}

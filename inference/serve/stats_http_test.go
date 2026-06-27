package serve

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// metricsServer starts a test HTTP server that answers /metrics with body and every other
// path with 404, modelling a runtime's metrics endpoint.
func metricsServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestPromStatsSourceScrapesAndMaps(t *testing.T) {
	srv := metricsServer(t, http.StatusOK, sampleLlamaMetrics)
	// The source is given the OpenAI base URL; it derives /metrics on the server root.
	s, err := LlamaCppStatsSource(nil).Stats(context.Background(), srv.URL+"/v1")
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if !s.Known || s.KVCacheUsage != 0.42 || s.RequestsRunning != 3 {
		t.Fatalf("snapshot = %+v, want the served reading", s)
	}
}

func TestPromStatsSourceVLLMConstructor(t *testing.T) {
	body := "vllm:gpu_cache_usage_perc 0.8\n" +
		"vllm:num_requests_running 4\n" +
		"vllm:num_requests_waiting 1\n"
	srv := metricsServer(t, http.StatusOK, body)
	s, err := VLLMStatsSource(nil).Stats(context.Background(), srv.URL+"/v1")
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if !s.Known || s.KVCacheUsage != 0.8 || s.RequestsRunning != 4 || s.RequestsWaiting != 1 {
		t.Fatalf("snapshot = %+v, want the served vLLM reading", s)
	}
}

func TestPromStatsSourceNon2xxErrors(t *testing.T) {
	srv := metricsServer(t, http.StatusServiceUnavailable, "down")
	if _, err := LlamaCppStatsSource(nil).Stats(context.Background(), srv.URL+"/v1"); err == nil {
		t.Fatal("a non-2xx metrics reply must be returned as an error")
	}
}

func TestPromStatsSourceReachableButUnmappedIsUnknown(t *testing.T) {
	// A 2xx reply that names no metric we map is a reachable runtime reporting nothing we
	// understand: unknown load, not an error.
	srv := metricsServer(t, http.StatusOK, "# only comments here\nsome_other_metric 1\n")
	s, err := LlamaCppStatsSource(nil).Stats(context.Background(), srv.URL+"/v1")
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if s.Known {
		t.Fatalf("expected unknown load, got %+v", s)
	}
}

func TestPromStatsSourceBoundsLargeBody(t *testing.T) {
	// The wanted metric sits at the top, then far more than the read cap follows. The read
	// is bounded, so this returns promptly with the early reading rather than ingesting an
	// unbounded body.
	body := "llamacpp:kv_cache_usage_ratio 0.5\n" +
		strings.Repeat("llamacpp:padding_metric 1\n", (maxMetricsBytes/26)+10_000)
	srv := metricsServer(t, http.StatusOK, body)
	s, err := LlamaCppStatsSource(nil).Stats(context.Background(), srv.URL+"/v1")
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if !s.Known || s.KVCacheUsage != 0.5 {
		t.Fatalf("snapshot = %+v, want the metric read before the cap", s)
	}
}

func TestPromStatsSourceCancelledContextErrors(t *testing.T) {
	srv := metricsServer(t, http.StatusOK, sampleLlamaMetrics)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := LlamaCppStatsSource(nil).Stats(ctx, srv.URL+"/v1"); err == nil {
		t.Fatal("a cancelled context must surface as an error, not a reading")
	}
}

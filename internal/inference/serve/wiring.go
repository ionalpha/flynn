package serve

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/ionalpha/flynn/sandbox"
)

// maxMetricsBytes caps a metrics read, so an unbounded or hostile body cannot exhaust
// memory during a scrape.
const maxMetricsBytes = 1 << 20

// PromStatsSource reads a runtime's Prometheus metrics over loopback and maps them onto
// a [RuntimeStats]. The metric names differ per runtime, so the mapping is injected. The
// read is time-boxed and size-bounded, so a hung or hostile endpoint cannot stall the
// caller.
type PromStatsSource struct {
	client *http.Client
	names  promNames
}

// LlamaCppStatsSource reads load stats from a llama.cpp server. A nil client gets a
// default with a short timeout.
func LlamaCppStatsSource(client *http.Client) PromStatsSource {
	return PromStatsSource{client: statsClient(client), names: llamaCppMetricNames}
}

// VLLMStatsSource reads load stats from a vLLM server. A nil client gets a default with
// a short timeout.
func VLLMStatsSource(client *http.Client) PromStatsSource {
	return PromStatsSource{client: statsClient(client), names: vllmMetricNames}
}

func statsClient(c *http.Client) *http.Client {
	if c != nil {
		return c
	}
	return &http.Client{Timeout: 2 * time.Second}
}

// Stats scrapes the metrics endpoint. A transport error, a non-2xx reply or a read error
// is returned for the manager to turn into an unknown reading; a 2xx body with none of
// the mapped metrics yields Known false.
func (s PromStatsSource) Stats(ctx context.Context, baseURL string) (RuntimeStats, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, metricsURL(baseURL), http.NoBody)
	if err != nil {
		return RuntimeStats{}, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return RuntimeStats{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return RuntimeStats{}, fmt.Errorf("serve: metrics endpoint returned status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxMetricsBytes))
	if err != nil {
		return RuntimeStats{}, err
	}
	return s.names.toStats(data), nil
}

var _ StatsSource = PromStatsSource{}

// SandboxLauncher is the production [Launcher]. The runtime parses untrusted weights, so
// it starts inside a Local sandbox, confined when the serve spec asks for it.
type SandboxLauncher struct {
	// SB is the sandbox. Its working directory confines the runtime and its environment
	// is the deny-by-default baseline.
	SB *sandbox.Local
}

// Serve starts the spec's command in the sandbox and returns it as a [Proc].
func (s SandboxLauncher) Serve(ctx context.Context, spec sandbox.ServeSpec) (Proc, error) {
	p, err := s.SB.Serve(ctx, spec)
	if err != nil {
		return nil, err
	}
	return p, nil
}

// HTTPProbe builds a [Prober] that requests the OpenAI-compatible model list. A loopback
// server answers that route once the weights are loaded, so a 2xx means ready. A short
// timeout keeps a hung connection from stalling the poll loop.
func HTTPProbe(client *http.Client) Prober {
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Second}
	}
	return func(ctx context.Context, baseURL string) error {
		reqCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, baseURL+"/models", http.NoBody)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode/100 != 2 {
			return fmt.Errorf("serve: endpoint returned status %d", resp.StatusCode)
		}
		return nil
	}
}

// EngineStopper is the production [ContainerStopper]. It drives the OCI engine with its
// own timeout, so `models stop` tears down a vLLM container as [OSKiller] stops a process.
func EngineStopper(engine, id string) error {
	return sandbox.StopContainer(context.Background(), sandbox.OCIEngine(engine), id)
}

// OSKiller is the production [Killer]. It stops a server an earlier invocation started,
// where only the pid survives in the registry.
func OSKiller(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if err := p.Kill(); err != nil {
		// An already-gone process is the success case for Stop.
		if errors.Is(err, os.ErrProcessDone) {
			return nil
		}
		return err
	}
	return nil
}

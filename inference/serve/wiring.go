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

// maxMetricsBytes caps how much of a runtime's metrics endpoint is read, so a runtime that
// streams an unbounded or hostile body cannot exhaust memory during a stats scrape.
const maxMetricsBytes = 1 << 20

// PromStatsSource reads a running runtime's Prometheus metrics endpoint over loopback and
// maps the named metric families onto a RuntimeStats snapshot. The metric names differ per
// runtime, so the mapping is injected: LlamaCppStatsSource and VLLMStatsSource supply the
// two dialects Flynn serves. The read is time-boxed and size-bounded, so a hung or hostile
// endpoint cannot stall or exhaust the caller.
type PromStatsSource struct {
	client *http.Client
	names  promNames
}

// LlamaCppStatsSource reads load stats from a llama.cpp server's metrics endpoint. A nil
// client gets a default with a short per-request timeout.
func LlamaCppStatsSource(client *http.Client) PromStatsSource {
	return PromStatsSource{client: statsClient(client), names: llamaCppMetricNames}
}

// VLLMStatsSource reads load stats from a vLLM server's metrics endpoint. A nil client
// gets a default with a short per-request timeout.
func VLLMStatsSource(client *http.Client) PromStatsSource {
	return PromStatsSource{client: statsClient(client), names: vllmMetricNames}
}

func statsClient(c *http.Client) *http.Client {
	if c != nil {
		return c
	}
	return &http.Client{Timeout: 2 * time.Second}
}

// Stats scrapes the runtime's metrics endpoint and maps it onto a snapshot. A transport
// error, a non-2xx reply, or a read error is returned as an error for the manager to turn
// into an unknown reading; a 2xx body that names none of the mapped metrics yields a
// snapshot with Known false.
func (s PromStatsSource) Stats(ctx context.Context, baseURL string) (RuntimeStats, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, metricsURL(baseURL), nil)
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

// SandboxLauncher starts a server process inside a Local sandbox, the production
// Launcher. The runtime parses untrusted weights, so running it through the sandbox
// rather than as a bare child is the point: confinement is applied per the Local's
// configuration when the serve spec requests it.
type SandboxLauncher struct {
	// SB is the sandbox the server runs inside. Its working directory is where the
	// runtime is confined and its environment is the deny-by-default baseline.
	SB *sandbox.Local
}

// Serve starts the spec's command in the sandbox and adapts the concrete process handle
// to the Proc the manager consumes.
func (s SandboxLauncher) Serve(ctx context.Context, spec sandbox.ServeSpec) (Proc, error) {
	p, err := s.SB.Serve(ctx, spec)
	if err != nil {
		return nil, err
	}
	return p, nil
}

// HTTPProbe builds a Prober that reports readiness by requesting the model list off the
// OpenAI-compatible endpoint. A loopback model server answers that route once it has
// loaded the weights and is ready to serve, so a 2xx is the readiness signal. The probe
// uses a short per-request timeout so a hung connection does not stall the poll loop.
func HTTPProbe(client *http.Client) Prober {
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Second}
	}
	return func(ctx context.Context, baseURL string) error {
		reqCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, baseURL+"/models", nil)
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

// EngineStopper stops a container-backed server by driving the OCI engine, the production
// ContainerStopper. It bounds the stop with its own timeout inside the sandbox helper, so a
// `models stop` for a vLLM container tears it down the same way OSKiller stops a process.
func EngineStopper(engine, id string) error {
	return sandbox.StopContainer(context.Background(), sandbox.OCIEngine(engine), id)
}

// OSKiller stops a process by pid through the operating system, the production Killer.
// It is used to stop a server a previous Flynn invocation started, where only the pid
// survives in the registry. Killing an already-gone process is reported by the OS and
// surfaced to the caller, which treats "no such process" as already stopped.
func OSKiller(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if err := p.Kill(); err != nil {
		// A process that is already gone is the success case for Stop, not a failure.
		if errors.Is(err, os.ErrProcessDone) {
			return nil
		}
		return err
	}
	return nil
}

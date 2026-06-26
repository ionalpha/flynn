package serve

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/ionalpha/flynn/sandbox"
)

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

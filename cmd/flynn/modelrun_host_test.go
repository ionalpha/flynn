package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/harness"
	"github.com/ionalpha/flynn/internal/inference/serve"
)

// aliveManager is a serve manager whose probe always answers, so a record put in its
// registry stays listed, and whose container stopper records what it was asked to stop.
func aliveManager(t *testing.T, stopped *[]string) (*serve.Manager, *serve.Registry) {
	t.Helper()
	reg := serve.NewRegistry(t.TempDir())
	mgr := serve.NewManager(
		&fakeServeLauncher{},
		func(context.Context, string) error { return nil },
		func(int) error { return nil },
		reg,
		serve.WithContainerStopper(func(engine, id string) error {
			*stopped = append(*stopped, engine+"/"+id)
			return nil
		}),
	)
	return mgr, reg
}

func TestRunnerWithNoPathAtAllSaysSo(t *testing.T) {
	r := &localRunner{dataDir: t.TempDir(), out: io.Discard}
	err := r.unavailable()
	if err == nil || !strings.Contains(err.Error(), "no provisioning path is configured") {
		t.Fatalf("unavailable = %v; want it to say no path is configured", err)
	}
}

func TestStopModelRoutesByWhatIsRunning(t *testing.T) {
	var stopped []string
	mgr, reg := aliveManager(t, &stopped)
	host, err := openHost(t.TempDir(), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Close() })
	r := &localRunner{dataDir: t.TempDir(), out: io.Discard, host: host, manager: mgr}

	// Nothing is running: the host answers, and finds nothing to stop.
	if found, err := r.stopModel(t.Context(), "qwen2.5:0.5b-instruct"); err != nil || found {
		t.Fatalf("stopModel with nothing running = %v, %v", found, err)
	}

	// A container record has no token file or host process, so the manager drives the engine.
	if err := reg.Put(serve.Record{ModelID: "vllm:test", ContainerID: "c1", Engine: "docker", Port: 9, BaseURL: "http://127.0.0.1:9/v1"}); err != nil {
		t.Fatal(err)
	}
	found, err := r.stopModel(t.Context(), "vllm:test")
	if err != nil || !found {
		t.Fatalf("stopModel of a container server = %v, %v", found, err)
	}
	if len(stopped) != 1 || stopped[0] != "docker/c1" {
		t.Fatalf("engine was asked to stop %v; want docker/c1", stopped)
	}

	// Without a host, the manager answers directly.
	r.host = nil
	if found, err := r.stopModel(t.Context(), "vllm:test"); err != nil || found {
		t.Fatalf("stopModel of a server already stopped = %v, %v", found, err)
	}
}

// A host that refuses to run the runtime stops the serve and provision paths before any
// download starts, and the refusal reaches the caller as the host worded it.
func TestHostRefusalReachesTheCaller(t *testing.T) {
	m, err := findLocalModel("qwen2.5:0.5b-instruct")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	r := newLocalRunner(t.TempDir(), &out)
	t.Cleanup(func() { _ = r.Close() })
	if r.host == nil {
		t.Fatalf("no host: %v", r.hostErr)
	}
	if err := r.host.RaiseFloor("999999999", "GHSA-test-0001"); err != nil {
		t.Fatal(err)
	}

	if _, err := r.serveModel(t.Context(), m, 0, false); err == nil || !strings.Contains(err.Error(), "GHSA-test-0001") {
		t.Fatalf("serveModel = %v; want the host's floor refusal", err)
	}
	if err := r.provision(t.Context(), m, m.Quants[0]); err == nil || !strings.Contains(err.Error(), "GHSA-test-0001") {
		t.Fatalf("provision = %v; want the host's floor refusal", err)
	}
	if strings.Contains(out.String(), "isolation:") {
		t.Fatalf("an isolation line was printed for a server that never started: %q", out.String())
	}
}

func TestOpenHostNeedsAUsableDataDirectory(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(blocker, "data")

	if _, err := openHost(dataDir, io.Discard); err == nil {
		t.Fatal("openHost accepted a data directory under a file")
	}
	if err := runRuntimeInstall(nil, dataDir, io.Discard); err == nil || !strings.Contains(err.Error(), "models install") {
		t.Fatalf("runRuntimeInstall = %v; want a models install error", err)
	}
	if err := runModelFetch([]string{"qwen2.5:0.5b-instruct"}, dataDir, io.Discard); err == nil {
		t.Fatal("runModelFetch fetched into a data directory it cannot use")
	}
}

func TestLocalModelClientUsesTheSuppliedHTTPClient(t *testing.T) {
	ep := serve.Endpoint{BaseURL: "http://127.0.0.1:1/v1"}
	if localModelClient(ep, "m", harness.Plan{}, nil) == nil {
		t.Fatal("no model built with the adapter's own client")
	}
	if localModelClient(ep, "m", harness.Plan{}, &http.Client{}) == nil {
		t.Fatal("no model built with the host's client")
	}
}

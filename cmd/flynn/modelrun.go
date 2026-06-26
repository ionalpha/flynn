package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/ionalpha/flynn/catalog"
	"github.com/ionalpha/flynn/fetch"
	"github.com/ionalpha/flynn/inference/launch"
	"github.com/ionalpha/flynn/inference/provision"
	"github.com/ionalpha/flynn/inference/serve"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/llm/openai"
	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/secret"
)

// selfProvisionedRuntime is the runtime Flynn fetches and runs itself, so a machine with
// no model tooling installed can still serve a local model. It is the engine the serve
// path provisions and gates; a model is served by pointing it at the model's weights.
const selfProvisionedRuntime = "llama.cpp"

// localRunner makes a catalog model serveable and reachable: it provisions the runtime
// and weights, builds a loopback serve plan, starts the server inside the sandbox, and
// hands back a running endpoint. Its provisioning and serving steps are fields so the
// whole lifecycle is exercised in tests with fakes and no live runtime; newLocalRunner
// wires the real implementations.
type localRunner struct {
	dataDir string
	out     io.Writer

	// ensureRuntime returns the path to a gated runtime server binary, provisioning it
	// if absent.
	ensureRuntime func(ctx context.Context, runtimeName string) (string, error)
	// ensureWeights returns the path to the verified weights for a quantization,
	// fetching them if absent.
	ensureWeights func(ctx context.Context, m catalog.ModelSpec, q catalog.Quant) (string, error)
	// manager starts, reuses, reports, and stops the server processes.
	manager *serve.Manager
	// freePort returns a free loopback TCP port for a new server.
	freePort func() (int, error)
}

// newLocalRunner builds a runner wired to the real runtime provisioner, weights fetcher,
// and a serve manager whose servers run inside a sandbox rooted under the data directory.
func newLocalRunner(dataDir string, out io.Writer) *localRunner {
	runDir := filepath.Join(dataDir, "run")
	// The sandbox confines the server to this directory, so it must exist before the
	// server is started (the process is launched with it as the working directory).
	_ = os.MkdirAll(runDir, 0o750)
	// The server runs inside a sandbox rooted at the run directory: confined to it for
	// writes, with the secure-by-default confinement the platform can enforce. The
	// runtime parses untrusted weights, so it is started behind that boundary rather
	// than as a bare child.
	sb, _ := sandbox.NewLocal(runDir, sandbox.WithDefaultConfinement())
	mgr := serve.NewManager(
		serve.SandboxLauncher{SB: sb},
		serve.HTTPProbe(nil),
		serve.OSKiller,
		serve.NewRegistry(runDir),
	)
	return &localRunner{
		dataDir:       dataDir,
		out:           out,
		ensureRuntime: realEnsureRuntime(dataDir, out),
		ensureWeights: realEnsureWeights(dataDir, out),
		manager:       mgr,
		freePort:      freeLoopbackPort,
	}
}

// serveModel runs the full lifecycle for a catalog model and returns a running endpoint.
// Provisioning a runtime or weights that are already present is a no-op, and a server
// that is already up is reused, so calling it repeatedly is cheap and idempotent.
func (r *localRunner) serveModel(ctx context.Context, m catalog.ModelSpec) (serve.Endpoint, error) {
	if !m.Local() {
		return serve.Endpoint{}, fmt.Errorf("%q is a hosted API model, not a local one", m.ID)
	}
	q, ok := m.SmallestQuant()
	if !ok {
		return serve.Endpoint{}, fmt.Errorf("%q has no quantization to serve", m.ID)
	}
	if q.URL == "" {
		return serve.Endpoint{}, fmt.Errorf("%q has no pinned direct download, so Flynn cannot fetch and serve it yet (the catalog entry records the model but no verifiable weights source)", m.ID)
	}

	binPath, err := r.ensureRuntime(ctx, selfProvisionedRuntime)
	if err != nil {
		return serve.Endpoint{}, err
	}
	weightsPath, err := r.ensureWeights(ctx, m, q)
	if err != nil {
		return serve.Endpoint{}, err
	}

	port, err := r.freePort()
	if err != nil {
		return serve.Endpoint{}, fmt.Errorf("pick a loopback port: %w", err)
	}
	// The trusted template decision is read from the weights with the hardened reader,
	// never the runtime's parser, and the plan forces the trusted template regardless.
	decision, err := launch.InspectTemplate(weightsPath, m.ChatTemplate)
	if err != nil {
		return serve.Endpoint{}, fmt.Errorf("inspect weights before serving: %w", err)
	}
	plan, err := launch.BuildPlan(launch.Config{
		BinPath:             binPath,
		WeightsPath:         weightsPath,
		Model:               m,
		Port:                port,
		CtxSize:             0,
		ModelEmbedsTemplate: decision.ModelSupplied,
	})
	if err != nil {
		return serve.Endpoint{}, err
	}

	return r.manager.Ensure(ctx, serve.EnsureConfig{
		ModelID: m.ID,
		Runtime: selfProvisionedRuntime,
		Plan:    plan,
		Confine: true,
	})
}

// realEnsureRuntime provisions and gates the self-provisioned runtime, reusing an
// already-installed build. It returns the path to the runnable server binary.
func realEnsureRuntime(dataDir string, out io.Writer) func(context.Context, string) (string, error) {
	return func(ctx context.Context, name string) (string, error) {
		rel, ok := provision.ReleaseFor(name, runtime.GOOS, runtime.GOARCH)
		if !ok {
			return "", fmt.Errorf("no pinned %s build for %s/%s", name, runtime.GOOS, runtime.GOARCH)
		}
		if err := rel.Gate(); err != nil {
			return "", fmt.Errorf("refusing to install %s %s: %w", name, rel.Version, err)
		}
		res, err := provision.Install(ctx, fetch.New(), rel, filepath.Join(dataDir, "runtimes"))
		if err != nil {
			return "", fmt.Errorf("provision %s: %w", name, err)
		}
		if !res.FromCache {
			_, _ = fmt.Fprintf(out, "provisioned %s %s (verified)\n", name, rel.Version)
		}
		return res.BinPath, nil
	}
}

// realEnsureWeights fetches and verifies a quantization's weights if they are not already
// on disk, returning their path. A present file is trusted because the fetch verified it
// against the catalog digest when it was written.
func realEnsureWeights(dataDir string, out io.Writer) func(context.Context, catalog.ModelSpec, catalog.Quant) (string, error) {
	return func(ctx context.Context, m catalog.ModelSpec, q catalog.Quant) (string, error) {
		if isCodeExecWeight(q.Format) {
			return "", fmt.Errorf("%q quant %q uses a code-executing weight format and will not be fetched", m.ID, q.Name)
		}
		dest := filepath.Join(dataDir, "models", weightsFileName(m.ID, q))
		if _, err := os.Stat(dest); err == nil {
			return dest, nil
		}
		_, _ = fmt.Fprintf(out, "fetching %s (%s, %s)\n", m.ID, q.Name, humanBytes(q.SizeBytes))
		res, err := fetch.New().Fetch(ctx, fetch.Request{
			URL:          q.URL,
			Dest:         dest,
			ExpectSHA256: q.Digest,
			MaxBytes:     sizeCeiling(q.SizeBytes),
		})
		if err != nil {
			return "", fmt.Errorf("fetch weights: %w", err)
		}
		return res.Path, nil
	}
}

// localModelClient builds an llm.Model that talks to a running local server over its
// loopback endpoint, reusing the OpenAI-compatible adapter. No credential is needed: the
// endpoint is loopback-only, and SafeBaseURL permits plaintext http there. The model
// name is the catalog id, which the runtime echoes back but does not route on, since it
// serves the single loaded model.
func localModelClient(ep serve.Endpoint, modelID string) llm.Model {
	return openai.New(secret.Text{}, openai.WithBaseURL(ep.BaseURL), openai.WithModel(modelID))
}

// freeLoopbackPort asks the OS for an unused loopback TCP port by binding port 0 and
// reading back the assignment, then releasing it for the server to claim. The brief gap
// between release and the server binding is the standard, accepted way to choose a port.
func freeLoopbackPort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// findLocalModel resolves a catalog id to a local model spec, with a clear error when the
// id is unknown or names a hosted model.
func findLocalModel(id string) (catalog.ModelSpec, error) {
	cat, err := catalog.Load()
	if err != nil {
		return catalog.ModelSpec{}, err
	}
	m, ok := findModel(cat, id)
	if !ok {
		return catalog.ModelSpec{}, fmt.Errorf("%q is not in the catalog (see `flynn models`)", id)
	}
	if !m.Local() {
		return catalog.ModelSpec{}, fmt.Errorf("%q is a hosted API model; select it with --model %s, there is nothing to run locally", id, id)
	}
	return m, nil
}

// resolveLocalModel provisions and serves a local catalog model, then returns an
// llm.Model bound to its loopback endpoint. It is the zero-touch resolution path for a
// local selection: a machine with nothing installed ends up talking to a running, gated,
// sandboxed model with no manual step. Progress is reported to stderr so it does not
// corrupt a piped run transcript on stdout.
func resolveLocalModel(ctx context.Context, modelSpec, dataDir string) (llm.Model, error) {
	m, err := findLocalModel(modelSpec)
	if err != nil {
		return nil, err
	}
	runner := newLocalRunner(dataDir, os.Stderr)
	ep, err := runner.serveModel(ctx, m)
	if err != nil {
		return nil, fmt.Errorf("serve local model %s: %w", modelSpec, err)
	}
	return localModelClient(ep, m.ID), nil
}

// isLocalModelID reports whether a model spec string names a local catalog model, so the
// goal path can route a local selection through the serve lifecycle instead of the
// hosted-provider resolver. An unknown id is not local.
func isLocalModelID(spec string) bool {
	cat, err := catalog.Load()
	if err != nil {
		return false
	}
	m, ok := findModel(cat, strings.TrimSpace(spec))
	return ok && m.Local()
}

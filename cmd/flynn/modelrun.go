package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/ionalpha/flynn/governed"

	"github.com/ionalpha/flynn/harness"
	"github.com/ionalpha/flynn/internal/bindguard"
	"github.com/ionalpha/flynn/internal/catalog"
	"github.com/ionalpha/flynn/internal/hardware"
	"github.com/ionalpha/flynn/internal/inference/launch"
	"github.com/ionalpha/flynn/internal/inference/modelsource"
	"github.com/ionalpha/flynn/internal/inference/serve"
	"github.com/ionalpha/flynn/internal/profilestore"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/llm/openai"
	"github.com/ionalpha/flynn/localmodel"
	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/secret"
)

// selfProvisionedRuntime is the runtime Flynn fetches and runs itself, so a machine with
// no model tooling can still serve a local model.
const selfProvisionedRuntime = "llama.cpp"

// localRunner makes a catalog model serveable: it classifies the source, gates it on the
// isolation this host provides, and returns a running endpoint. [localmodel.Host] owns
// the advisory gate, digest checks, loopback pin, chat-template override, sandbox and
// per-launch token for llama.cpp. This type adds consent and risk presentation, the
// vLLM container path, and the pool's process management.
type localRunner struct {
	dataDir string
	out     io.Writer

	// host serves the llama.cpp path. A test that stubs the process path leaves it nil and
	// sets ensureRuntime.
	host *localmodel.Host
	// hostErr says why host is nil, so a runner that could not build one refuses to serve
	// instead of falling back to an unguarded path.
	hostErr error

	// ensureRuntime and ensureWeights replace the host's provisioning in tests. serveModel
	// never reaches them when host is set.
	ensureRuntime func(ctx context.Context, runtimeName string) (string, error)
	ensureWeights func(ctx context.Context, m catalog.ModelSpec, q catalog.Quant) (string, error)
	// manager lists, adopts and stops servers. Its probes and stats readers use the host's
	// client, which presents the per-launch token.
	manager *serve.Manager

	freePort func() (int, error)
	// sb is the servers' sandbox; the trust gate measures a model's required isolation
	// against its containment level.
	sb *sandbox.Local
	// ledger records every model source's provenance and pins its integrity on first use.
	ledger *modelsource.Ledger
	// box gates the GPU path (a vLLM container needs a GPU and an engine with the NVIDIA
	// toolkit) and picks the quant to serve.
	box hardware.Box
}

// Close releases the sandbox's operating-system objects (on Windows, a container
// profile), which would otherwise leak once per invocation. A server left running keeps
// the handles it holds, so a teardown failure does not fail the command.
func (r *localRunner) Close() error {
	if r == nil {
		return nil
	}
	var errs []error
	if r.host != nil {
		errs = append(errs, r.host.Close())
	}
	if r.sb != nil {
		errs = append(errs, r.sb.Close())
	}
	return errors.Join(errs...)
}

// newLocalRunner builds a runner whose servers run in a sandbox under the data directory.
func newLocalRunner(dataDir string, out io.Writer) *localRunner {
	// The host creates the data tree owner-only, so it opens before anything below writes.
	host, hostErr := openHost(dataDir, out)
	runDir := filepath.Join(dataDir, "run")
	// The server is launched with this directory as its working directory.
	_ = os.MkdirAll(runDir, 0o750)
	// The runtime parses untrusted weights, so it runs confined to the run directory with
	// the strongest default confinement the platform enforces.
	sb, _ := sandbox.NewLocal(runDir, sandbox.WithDefaultConfinement())
	var client *http.Client
	if host != nil {
		client = host.Client()
	}
	mgr := serve.NewManager(
		serve.SandboxLauncher{SB: sb},
		serve.HTTPProbe(client),
		serve.OSKiller,
		serve.NewRegistry(runDir),
		// The runtime's Prometheus endpoint drives scheduling and reporting. It requires the
		// launch token, which the host's client presents.
		serve.WithStatsSource(selfProvisionedRuntime, serve.LlamaCppStatsSource(client)),
		// A vLLM container has no host pid, so its teardown drives the engine.
		serve.WithContainerStopper(serve.EngineStopper),
		serve.WithStatsSource("vllm", serve.VLLMStatsSource(nil)),
	)
	return &localRunner{
		dataDir:  dataDir,
		out:      out,
		host:     host,
		hostErr:  hostErr,
		manager:  mgr,
		freePort: bindguard.FreeLoopbackPort,
		sb:       sb,
		ledger:   modelsource.NewLedger(filepath.Join(dataDir, "models")),

		box: hardware.Detect(context.Background()),
	}
}

func openHost(dataDir string, out io.Writer) (*localmodel.Host, error) {
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, fmt.Errorf("resolve the data directory: %w", err)
	}
	// gateAndServe already decided from trust and isolation whether the model may run, so
	// the host only reports what the sandbox achieved and a platform limited to a process
	// jail is not refused twice.
	// The host gets the catalog with user entries, so a `flynn models add` model is fetched,
	// digest-checked and served like a curated one. It classifies a user entry by its
	// origin and admits it only under the isolation that origin needs.
	cat, _, _ := catalog.LoadWithOverlay(abs)
	return localmodel.Open(localmodel.Config{DataDir: abs, Catalog: cat.Models, Progress: out, MinContainment: "process"})
}

// httpClient presents the server's per-launch token and reaches loopback only. Nil means
// a stubbed test runner with no host.
func (r *localRunner) httpClient() *http.Client {
	if r.host == nil {
		return nil
	}
	return r.host.Client()
}

// stopModel ends a model's server. A process server goes through the host, which deletes
// its token file; a vLLM container goes through the manager.
func (r *localRunner) stopModel(ctx context.Context, id string) (bool, error) {
	if r.host == nil {
		return r.manager.Stop(id)
	}
	live, err := r.manager.Status(ctx)
	if err != nil {
		return false, err
	}
	for _, rec := range live {
		if rec.ModelID == id && rec.ContainerID != "" {
			return r.manager.Stop(id)
		}
	}
	return r.host.Stop(id)
}

// consentPolicy is how gateAndServe obtains consent for a source that is not a vetted
// catalog entry. A consent-free path is an explicit, reviewable value.
type consentPolicy int

const (
	// consentPrompt shows the risk and asks, defaulting to "no"; a non-interactive session
	// is refused.
	consentPrompt consentPolicy = iota
	// consentPreapproved records an up-front --yes instead of prompting. The risk is still
	// shown.
	consentPreapproved
	// consentTrustGateOnly classifies, records provenance and applies the containment gate
	// without showing risk or prompting. The goal path uses it: catalog models are not
	// risky, and a user entry consented at `flynn models add`. A new non-catalog source on
	// that path must add its consent surface here.
	consentTrustGateOnly
)

func consentFor(autoApprove bool) consentPolicy {
	if autoApprove {
		return consentPreapproved
	}
	return consentPrompt
}

// gateAndServe is the one admission path for a local model reference, shared by `models
// run`, `models probe` and the goal path: parse, classify (refusing unsafe weight
// formats, recording provenance), show the risk, gate on isolation, obtain consent,
// then provision and serve. A source whose trust needs more isolation than the host has
// is refused. ctxSize and cpuOnly (0 and false by default) let the pool's degraded
// relaunch shrink a model that ran out of memory.
func (r *localRunner) gateAndServe(ctx context.Context, id string, consent consentPolicy, ctxSize int, cpuOnly bool) (catalog.ModelSpec, serve.Endpoint, error) {
	src, err := parseModelRef(id)
	if err != nil {
		return catalog.ModelSpec{}, serve.Endpoint{}, err
	}
	_, userEntry := userModelEntry(id)
	class, err := r.classifySource(src)
	if err != nil {
		return catalog.ModelSpec{}, serve.Endpoint{}, err
	}
	// The risk is shown before the gate, so a refusal is explained.
	rs := r.riskSurface(src, class)
	if consent != consentTrustGateOnly {
		printRiskSurface(r.out, rs)
	}
	if err := r.admitOnly(class.Trust); err != nil {
		return catalog.ModelSpec{}, serve.Endpoint{}, err
	}
	// Consent defaults to no, and a non-interactive session refuses.
	if consent != consentTrustGateOnly {
		if err := requireConsent(rs, stdinIsTerminal(), consent == consentPreapproved, os.Stdin, r.out); err != nil {
			return catalog.ModelSpec{}, serve.Endpoint{}, err
		}
	}
	if src.Kind != modelsource.KindCatalog && !userEntry {
		// Admitted and consented but not curated: non-catalog serving waits on the strong
		// isolation tier it needs.
		return catalog.ModelSpec{}, serve.Endpoint{}, fmt.Errorf("%s is %s and would run, but serving a non-catalog model is not wired yet; only catalog models serve today", src.Raw, class.Trust)
	}
	m, err := findLocalModel(id)
	if err != nil {
		return catalog.ModelSpec{}, serve.Endpoint{}, err
	}
	ep, err := r.serveModel(ctx, m, ctxSize, cpuOnly)
	if err != nil {
		return catalog.ModelSpec{}, serve.Endpoint{}, err
	}
	return m, ep, nil
}

// classifySource classifies a source, refuses an unsafe weight format and records
// provenance without gating, so a command can explain the risk before it refuses.
func (r *localRunner) classifySource(src modelsource.Source) (modelsource.Classification, error) {
	class := modelsource.Classify(src, r.knownPublisher)

	// Code-executing and unrecognized formats are refused for any source. A catalog or bare
	// hub reference is checked once its weights file is resolved.
	if name := sourceFileName(src); name != "" {
		if err := modelsource.CheckRunnableFormat(name); err != nil {
			return class, err
		}
	}

	if r.ledger != nil {
		// Recorded before anything runs; the digest is added after the fetch verifies.
		_ = r.ledger.Record(modelsource.Provenance{
			Key:   src.Key(),
			Raw:   src.Raw,
			Trust: class.Trust.String(),
		})
	}
	return class, nil
}

// admitOnly applies the containment gate alone and names the trust, the isolation it
// needs and what the host provides. It has no side effects, so it also answers whether
// a model could run.
func (r *localRunner) admitOnly(trust sandbox.Trust) error {
	if err := sandbox.Admit(r.sb, trust); err != nil {
		return fmt.Errorf("%s work needs %s isolation, but this host provides only %s; refusing to run it unsafely",
			trust, sandbox.Required(trust), sandbox.ContainmentOf(r.sb))
	}
	return nil
}

// riskSurface fills in integrity from what is known of the bytes: a catalog entry is
// pinned ahead of time, a source pinned on first use is trust-on-first-use, and anything
// else is unverified.
func (r *localRunner) riskSurface(src modelsource.Source, class modelsource.Classification) modelsource.RiskSurface {
	integ := modelsource.IntegrityUnverified
	switch {
	case src.Kind == modelsource.KindCatalog:
		integ = modelsource.IntegrityPinned
	case r.ledger != nil:
		if d, ok, _ := r.ledger.PinnedDigest(src.Key()); ok && d != "" {
			integ = modelsource.IntegrityTOFU
		}
	}
	return modelsource.DescribeRisk(src, class, integ)
}

// knownPublisher counts the curated set and the embedded catalog's publishers as
// recognized.
func (r *localRunner) knownPublisher(owner string) bool {
	return modelsource.KnownPublisher(owner, catalogPublishers()...)
}

// sourceFileName returns the weights file a source names, or "" when the reference
// alone does not say.
func sourceFileName(src modelsource.Source) string {
	switch src.Kind {
	case modelsource.KindHuggingFace:
		return src.File
	case modelsource.KindURL:
		return src.URL
	case modelsource.KindFile:
		return src.Path
	default:
		return ""
	}
}

func catalogPublishers() []string {
	cat, err := catalog.Load()
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, m := range cat.Models {
		p := m.Source.Publisher
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

// serveModel provisions and serves a catalog model. Present runtimes, weights and
// running servers are reused, so repeat calls are cheap. ctxSize caps the context
// window (0 is the runtime default) and cpuOnly forces CPU, for a model that ran out of
// memory at full size.
func (r *localRunner) serveModel(ctx context.Context, m catalog.ModelSpec, ctxSize int, cpuOnly bool) (serve.Endpoint, error) {
	if !m.Local() {
		return serve.Endpoint{}, fmt.Errorf("%q is a hosted API model, not a local one", m.ID)
	}
	// A safetensors quant runs on the vLLM GPU container and a GGUF quant on llama.cpp. A
	// model with both uses GGUF when there is no GPU container path.
	gpuContainer := r.box.HasGPU() && r.box.Containers.GPUPassthrough()
	q, engine, err := selectServeQuant(m, gpuContainer)
	if err != nil {
		return serve.Endpoint{}, err
	}
	if engine == launch.EngineVLLM {
		return r.serveContainerModel(ctx, m, q, ctxSize)
	}

	if q.URL == "" {
		return serve.Endpoint{}, fmt.Errorf("%q has no pinned direct download, so Flynn cannot fetch and serve it yet (the catalog entry records the model but no verifiable weights source)", m.ID)
	}

	switch {
	case r.host != nil:
		return r.serveViaHost(ctx, m, ctxSize, cpuOnly)
	case r.ensureRuntime == nil || r.ensureWeights == nil:
		// No host and no stubs: refuse to start a server without the guards.
		return serve.Endpoint{}, r.unavailable()
	}
	return r.serveStubbed(ctx, m, q, ctxSize, cpuOnly)
}

// provision installs the runtime and fetches weights without serving. The host path
// gates the runtime and checks both digests.
func (r *localRunner) provision(ctx context.Context, m catalog.ModelSpec, q catalog.Quant) error {
	if r.host != nil {
		if _, err := r.host.EnsureRuntime(ctx); err != nil {
			return err
		}
		_, err := r.host.EnsureWeights(ctx, m.ID, q.Name)
		return err
	}
	if r.ensureRuntime == nil || r.ensureWeights == nil {
		return r.unavailable()
	}
	if _, err := r.ensureRuntime(ctx, selfProvisionedRuntime); err != nil {
		return err
	}
	_, err := r.ensureWeights(ctx, m, q)
	return err
}

func (r *localRunner) unavailable() error {
	if r.hostErr != nil {
		return fmt.Errorf("local inference is unavailable: %w", r.hostErr)
	}
	return errors.New("local inference is unavailable: no provisioning path is configured")
}

// serveViaHost serves through [localmodel.Host], which applies every guard listed on
// [localRunner].
func (r *localRunner) serveViaHost(ctx context.Context, m catalog.ModelSpec, ctxSize int, cpuOnly bool) (serve.Endpoint, error) {
	ep, err := r.host.Serve(ctx, m.ID, localmodel.ServeOptions{CtxSize: ctxSize, CPUOnly: cpuOnly})
	if err != nil {
		return serve.Endpoint{}, err
	}
	if r.out != nil && !ep.Reused {
		_, _ = fmt.Fprintf(r.out, "isolation: %s\n", ep.Containment)
	}
	return serve.Endpoint{ModelID: ep.ModelID, BaseURL: ep.BaseURL, Port: ep.Port, PID: ep.PID, Reused: ep.Reused}, nil
}

// serveStubbed is the process path with injected provisioning, for tests. newLocalRunner
// never reaches it.
func (r *localRunner) serveStubbed(ctx context.Context, m catalog.ModelSpec, q catalog.Quant, ctxSize int, cpuOnly bool) (serve.Endpoint, error) {
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
	decision, err := launch.InspectTemplate(weightsPath, m.ChatTemplate)
	if err != nil {
		return serve.Endpoint{}, fmt.Errorf("inspect weights before serving: %w", err)
	}
	plan, err := launch.BuildPlan(launch.Config{
		BinPath:             binPath,
		WeightsPath:         weightsPath,
		Model:               m,
		Format:              q.Format,
		Port:                port,
		CtxSize:             ctxSize,
		CPUOnly:             cpuOnly,
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

// localModelClient builds an OpenAI-compatible llm.Model for a local loopback server.
// httpc presents the per-launch token; nil suits a stubbed server. SafeBaseURL permits
// plaintext http on loopback. The runtime serves one model, so the catalog id is a
// label it echoes and does not route on.
func localModelClient(ep serve.Endpoint, modelID string, plan harness.Plan, httpc *http.Client) llm.Model {
	opts := []openai.Option{openai.WithBaseURL(ep.BaseURL), openai.WithModel(modelID)}
	if httpc != nil {
		opts = append(opts, openai.WithHTTPClient(httpc))
	}
	// A model the plan does not trust with tool calls is constrained to a grammar, so a
	// malformed call cannot be produced. Backends without grammar support ignore it.
	if plan.ConstrainToolCalls {
		opts = append(opts, openai.WithToolGrammar())
	}
	return openai.New(secret.Text{}, opts...)
}

// findLocalModel errors when the id is unknown or names a hosted model.
func findLocalModel(id string) (catalog.ModelSpec, error) {
	cat, err := loadCatalog()
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

// resolveLocalModel provisions and serves a local catalog model and binds an llm.Model
// to it, with no manual step. Progress goes to stderr so a piped transcript on stdout
// stays clean.
func resolveLocalModel(ctx context.Context, modelSpec, dataDir string) (llm.Model, harness.Plan, error) {
	runner := newLocalRunner(dataDir, os.Stderr)
	defer func() { _ = runner.Close() }()
	// The goal path uses the same gate as `models run`. Only catalog models resolve here,
	// so there is no consent surface.
	m, ep, err := runner.gateAndServe(ctx, modelSpec, consentTrustGateOnly, 0, false)
	if err != nil {
		return nil, harness.Plan{}, fmt.Errorf("local model %s: %w", modelSpec, err)
	}
	plan := localModelPlan(ctx, m, dataDir)
	logPlan(os.Stderr, m.ID, plan)
	return localModelClient(ep, m.ID, plan, runner.httpClient()), plan, nil
}

// localModelPlan gives an unmeasured model the most conservative plan; only a measured
// profile earns the lean path. The advertised context window caps the plan's context.
func localModelPlan(ctx context.Context, m catalog.ModelSpec, dataDir string) harness.Plan {
	return harness.PlanFor(localProfileSource(ctx, dataDir), m.ID, m.ContextTokens)
}

// localProfileSource reads the profiles `flynn models probe` measured from a store
// snapshot and holds nothing open. Any failure yields an empty source, so every model
// resolves as unknown and the lookup never blocks a run.
func localProfileSource(ctx context.Context, dataDir string) harness.ProfileSource {
	// No store yet means nothing was measured. A read-only lookup must not create one.
	if dsn := dataStoreFile(dataDir); dsn == "" {
		return harness.StaticProfiles{}
	} else if _, err := os.Stat(dsn); err != nil {
		return harness.StaticProfiles{}
	}
	store, err := openDataStore(ctx, dataDir)
	if err != nil {
		return harness.StaticProfiles{}
	}
	defer func() { _ = store.Close() }()
	reg, err := governed.Registry()
	if err != nil {
		return harness.StaticProfiles{}
	}
	src, err := profilestore.NewSource(ctx, store.Resources(reg))
	if err != nil {
		return harness.StaticProfiles{}
	}
	return src
}

// logPlan writes the model's scaffolding to the progress stream, so a run records why
// it was or was not helped.
func logPlan(w io.Writer, modelID string, plan harness.Plan) {
	_, _ = fmt.Fprintf(w, "harness plan for %s: constrain=%t simplify=%t verify=%d maxContext=%d\n",
		modelID, plan.ConstrainToolCalls, plan.SimplifyToolSchemas, plan.VerifyPasses, plan.MaxContext)
}

// isLocalModelID reports whether a spec names a local catalog model, user-added ones
// included, so the goal path routes it to the serve lifecycle. Whether it is vetted is
// decided by [parseModelRef] and the admission gate.
func isLocalModelID(spec string) bool {
	cat, err := loadCatalog()
	if err != nil {
		return false
	}
	m, ok := findModel(cat, strings.TrimSpace(spec))
	return ok && m.Local()
}

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
// no model tooling installed can still serve a local model. It is the engine the serve
// path provisions and gates; a model is served by pointing it at the model's weights.
const selfProvisionedRuntime = "llama.cpp"

// localRunner makes a catalog model serveable and reachable: it classifies the source, gates
// it on the isolation this host provides, and hands back a running endpoint. A llama.cpp
// model is provisioned and served through [localmodel.Host], the public API that owns the
// advisory gate, the digest checks, the loopback pin, the chat-template override, the
// sandbox and the per-launch token. What stays here is what that API does not cover:
// consent and risk presentation, the vLLM container path, and the pool's process management.
type localRunner struct {
	dataDir string
	out     io.Writer

	// host provisions and serves the llama.cpp process path. newLocalRunner sets it; a test
	// that stubs the process path leaves it nil and sets ensureRuntime instead.
	host *localmodel.Host
	// hostErr says why host is nil, so a runner that could not build one refuses to serve
	// instead of falling back to an unguarded path.
	hostErr error

	// ensureRuntime and ensureWeights stand in for the host's provisioning in tests. They
	// are nil in a runner built by newLocalRunner, and serveModel never reaches them when
	// host is set.
	ensureRuntime func(ctx context.Context, runtimeName string) (string, error)
	ensureWeights func(ctx context.Context, m catalog.ModelSpec, q catalog.Quant) (string, error)
	// manager lists, adopts, and stops server processes and containers. Its probes and
	// stats readers use the host's client, which presents the per-launch token.
	manager *serve.Manager
	// freePort returns a free loopback TCP port for a new server.
	freePort func() (int, error)
	// sb is the sandbox the servers run inside; its containment level is what the trust
	// gate measures a model's required isolation against.
	sb *sandbox.Local
	// ledger records the provenance of every model source and pins its integrity on
	// first use.
	ledger *modelsource.Ledger
	// box is the detected hardware: it gates the GPU serving path (a vLLM container needs a
	// GPU plus a container engine with the NVIDIA toolkit) and picks the quant to serve.
	box hardware.Box
}

// Close releases the runner's sandbox once the command that built it is done. The sandbox
// registers operating-system objects for the confined servers it starts (on Windows, a
// container profile), and a command that returns without releasing them leaves one behind
// on every invocation. A server the runner left running keeps the handles it already holds,
// so a teardown failure here is not worth failing the command over.
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

// newLocalRunner builds a runner over the public local-inference API and a serve manager
// whose servers run inside a sandbox rooted under the data directory.
func newLocalRunner(dataDir string, out io.Writer) *localRunner {
	// The host is opened first: it creates the data tree owner-only, so the creations below
	// find it in place.
	host, hostErr := openHost(dataDir, out)
	runDir := filepath.Join(dataDir, "run")
	// The sandbox confines the server to this directory, so it must exist before the
	// server is started (the process is launched with it as the working directory).
	_ = os.MkdirAll(runDir, 0o750)
	// The server runs inside a sandbox rooted at the run directory: confined to it for
	// writes, with the secure-by-default confinement the platform can enforce. The
	// runtime parses untrusted weights, so it is started behind that boundary rather
	// than as a bare child.
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
		// The self-provisioned runtime exposes a Prometheus endpoint, so its load can be
		// read back to drive scheduling and reporting. It requires the launch token, which
		// the host's client presents.
		serve.WithStatsSource(selfProvisionedRuntime, serve.LlamaCppStatsSource(client)),
		// A container-backed server (vLLM) is stopped by driving the engine, since it has no
		// host pid; the manager routes a container record's teardown here.
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
		// Detect the hardware once so the serve path can choose the runtime that fits.
		box: hardware.Detect(context.Background()),
	}
}

// openHost opens the public local-inference host over the data directory, with its
// progress lines going to out.
func openHost(dataDir string, out io.Writer) (*localmodel.Host, error) {
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, fmt.Errorf("resolve the data directory: %w", err)
	}
	// The admission gate in gateAndServe has already decided, from the trust level and the
	// isolation this host provides, whether the model may run, and told the user. The host
	// is asked only to report what the sandbox achieved, so a platform that cannot confine
	// further than a process jail is not refused a second time.
	return localmodel.Open(localmodel.Config{DataDir: abs, Progress: out, MinContainment: "process"})
}

// httpClient is the client for a running local server. It presents the server's per-launch
// token and reaches loopback only. A runner with no host returns nil, which leaves the
// OpenAI adapter on its own client; that is the stubbed-test case, never the production one.
func (r *localRunner) httpClient() *http.Client {
	if r.host == nil {
		return nil
	}
	return r.host.Client()
}

// stopModel ends the server for a model. A process server goes through the host, which also
// deletes its token file; a container record (vLLM) has no token file and goes through the
// manager, which drives the engine.
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

// consentPolicy says how gateAndServe obtains consent for a source that is not a
// vetted catalog entry. It is an explicit value so a consent-free path is a
// deliberate, reviewable choice rather than a missing call.
type consentPolicy int

const (
	// consentPrompt shows the risk surface and requires explicit consent for a risky
	// source: an interactive session is prompted with "no" as the default, a
	// non-interactive one is refused.
	consentPrompt consentPolicy = iota
	// consentPreapproved records an explicit up-front approval (--yes) instead of
	// prompting. The risk surface is still shown.
	consentPreapproved
	// consentTrustGateOnly applies classification, provenance recording, and the
	// containment gate, but neither prints the risk surface nor prompts. The goal
	// path uses it because only catalog models resolve there today and a catalog
	// model is not risky; if non-catalog sources ever resolve through that path,
	// this value is the grep target for adding its consent surface.
	consentTrustGateOnly
)

// consentFor maps a command's --yes flag to the consent policy the gate applies.
func consentFor(autoApprove bool) consentPolicy {
	if autoApprove {
		return consentPreapproved
	}
	return consentPrompt
}

// gateAndServe is the one admission-and-serve path for a local model reference:
// parse, classify (refusing an unsafe weight format, recording provenance), show
// the risk, gate on the isolation this host provides, obtain consent per policy,
// then provision and serve. Every route that serves a model goes through it -
// `models run`, `models probe`, and the goal path - so a check added here covers
// all of them instead of living in per-command copies. A source whose trust
// requires stronger isolation than is available is refused rather than run, which
// is the security guarantee for a model whose bytes we cannot vouch for. Callers
// wrap the returned error with their command name. ctxSize and cpuOnly pass
// through to the serve (0, false for the defaults); the pool's degraded relaunch
// uses them to shrink a model that ran out of memory.
func (r *localRunner) gateAndServe(ctx context.Context, id string, consent consentPolicy, ctxSize int, cpuOnly bool) (catalog.ModelSpec, serve.Endpoint, error) {
	src, err := modelsource.Parse(id, isLocalModelID)
	if err != nil {
		return catalog.ModelSpec{}, serve.Endpoint{}, err
	}
	class, err := r.classifySource(src)
	if err != nil {
		return catalog.ModelSpec{}, serve.Endpoint{}, err
	}
	// Surface the trust, isolation, integrity, and network posture in plain language
	// before anything happens, so a refusal below is explained rather than bare.
	rs := r.riskSurface(src, class)
	if consent != consentTrustGateOnly {
		printRiskSurface(r.out, rs)
	}
	if err := r.admitOnly(class.Trust); err != nil {
		return catalog.ModelSpec{}, serve.Endpoint{}, err
	}
	// Require explicit consent for anything that is not a vetted catalog model. The
	// safe answer is the default, and a non-interactive session refuses rather than
	// assumes yes.
	if consent != consentTrustGateOnly {
		if err := requireConsent(rs, stdinIsTerminal(), consent == consentPreapproved, os.Stdin, r.out); err != nil {
			return catalog.ModelSpec{}, serve.Endpoint{}, err
		}
	}
	if src.Kind != modelsource.KindCatalog {
		// The source is admitted by the isolation gate and consented to, but is not a
		// curated catalog entry. Serving an arbitrary downloaded model is delivered
		// with the strong isolation tier it requires; until then the gate above
		// refuses an uncontained run.
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

// classifySource classifies a source, refuses an unsafe weight format, and records the
// source's provenance, without gating it. Splitting the classification from the gate lets
// a command show the risk in plain language before it refuses, so a refusal is explained
// rather than bare.
func (r *localRunner) classifySource(src modelsource.Source) (modelsource.Classification, error) {
	class := modelsource.Classify(src, r.knownPublisher)

	// Refuse a code-executing or unrecognized weight format up front, for any source.
	// Only the file-bearing references name a file to check here; a catalog or bare hub
	// repo reference is format-checked when its concrete weights file is resolved.
	if name := sourceFileName(src); name != "" {
		if err := modelsource.CheckRunnableFormat(name); err != nil {
			return class, err
		}
	}

	if r.ledger != nil {
		// Record the source and its classification for audit before anything runs. The
		// digest is filled in later, once the weights are fetched and verified.
		_ = r.ledger.Record(modelsource.Provenance{
			Key:   src.Key(),
			Raw:   src.Raw,
			Trust: class.Trust.String(),
		})
	}
	return class, nil
}

// admitOnly applies just the containment gate for a trust level against the serving
// sandbox, returning a plain-language refusal that names the trust, the isolation it
// needs, and what the host provides. It has no side effects, so it is also used to report
// whether a model could run without running or recording anything.
func (r *localRunner) admitOnly(trust sandbox.Trust) error {
	if err := sandbox.Admit(r.sb, trust); err != nil {
		return fmt.Errorf("%s work needs %s isolation, but this host provides only %s; refusing to run it unsafely",
			trust, sandbox.Required(trust), sandbox.ContainmentOf(r.sb))
	}
	return nil
}

// riskSurface builds the plain-language risk summary for a classified source, filling in
// the integrity from what is known about its bytes: a catalog entry is pinned ahead of
// time, a source whose digest was pinned on first use is trust-on-first-use, and anything
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

// knownPublisher reports whether a hub owner is a recognized first-party publisher,
// counting both the curated set and the publishers named in the embedded catalog, so a
// publisher a curator already vouched for is recognized.
func (r *localRunner) knownPublisher(owner string) bool {
	return modelsource.KnownPublisher(owner, catalogPublishers()...)
}

// sourceFileName returns the weights file name a source names, for the format guard, or
// empty when the concrete file is not known from the reference alone.
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

// catalogPublishers returns the distinct publishers named in the embedded catalog, so a
// hub model from a publisher the catalog already trusts is recognized.
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

// serveModel runs the full lifecycle for a catalog model and returns a running endpoint.
// Provisioning a runtime or weights that are already present is a no-op, and a server
// that is already up is reused, so calling it repeatedly is cheap and idempotent. ctxSize
// caps the context window (0 leaves the runtime's default) and cpuOnly forces a CPU run; both
// are the recovery path for a model that ran out of memory at full size.
func (r *localRunner) serveModel(ctx context.Context, m catalog.ModelSpec, ctxSize int, cpuOnly bool) (serve.Endpoint, error) {
	if !m.Local() {
		return serve.Endpoint{}, fmt.Errorf("%q is a hosted API model, not a local one", m.ID)
	}
	// Pick the quant and the engine that fit this host: a safetensors quant runs on the vLLM
	// GPU container path, a GGUF quant on the llama.cpp process path. A model with both falls
	// back to GGUF when there is no GPU container path.
	gpuContainer := r.box.HasGPU() && r.box.Containers.GPUPassthrough()
	q, engine, err := selectServeQuant(m, gpuContainer)
	if err != nil {
		return serve.Endpoint{}, err
	}
	if engine == launch.EngineVLLM {
		return r.serveContainerModel(ctx, m, q, ctxSize)
	}

	// The llama.cpp process path serves a single-file GGUF weight.
	if q.URL == "" {
		return serve.Endpoint{}, fmt.Errorf("%q has no pinned direct download, so Flynn cannot fetch and serve it yet (the catalog entry records the model but no verifiable weights source)", m.ID)
	}

	switch {
	case r.host != nil:
		return r.serveViaHost(ctx, m, ctxSize, cpuOnly)
	case r.ensureRuntime == nil || r.ensureWeights == nil:
		// No host and no stubs: refuse rather than start a server without the guards.
		return serve.Endpoint{}, r.unavailable()
	}
	return r.serveStubbed(ctx, m, q, ctxSize, cpuOnly)
}

// provision installs the runtime and fetches the weights without starting a server. The host
// path gates the runtime and checks both digests; the stubbed path calls the injected steps.
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

// unavailable is the refusal for a runner that has neither a host nor injected steps.
func (r *localRunner) unavailable() error {
	if r.hostErr != nil {
		return fmt.Errorf("local inference is unavailable: %w", r.hostErr)
	}
	return errors.New("local inference is unavailable: no provisioning path is configured")
}

// serveViaHost serves a catalog model through the public API. The host applies the advisory
// gate, the digest checks before and after the header parse, the loopback pin, the
// chat-template override, the per-launch token and the sandbox; none of that is repeated here.
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

// serveStubbed is the process path with provisioning injected, for tests that exercise the
// lifecycle with no runtime. It is unreachable from newLocalRunner, which always sets host or
// leaves both stubs nil.
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

// localModelClient builds an llm.Model that talks to a running local server over its
// loopback endpoint, reusing the OpenAI-compatible adapter. The server demands a per-launch
// token, which httpc presents (the host's client adds it for the port it addresses); a nil
// httpc leaves the adapter on its own client, for a stubbed server that needs none.
// SafeBaseURL permits plaintext http on loopback. The model name is the catalog id, which
// the runtime echoes back but does not route on, since it serves the single loaded model.
func localModelClient(ep serve.Endpoint, modelID string, plan harness.Plan, httpc *http.Client) llm.Model {
	opts := []openai.Option{openai.WithBaseURL(ep.BaseURL), openai.WithModel(modelID)}
	if httpc != nil {
		opts = append(opts, openai.WithHTTPClient(httpc))
	}
	// A model whose tool-call reliability the plan does not trust is constrained to a
	// grammar so a malformed call is structurally impossible. The local runtime honors the
	// grammar request field; the option is a no-op against a backend that does not.
	if plan.ConstrainToolCalls {
		opts = append(opts, openai.WithToolGrammar())
	}
	return openai.New(secret.Text{}, opts...)
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
func resolveLocalModel(ctx context.Context, modelSpec, dataDir string) (llm.Model, harness.Plan, error) {
	runner := newLocalRunner(dataDir, os.Stderr)
	defer func() { _ = runner.Close() }()
	// The goal path goes through the same admission gate as `models run`, so no
	// model-run route skips classification, provenance, or the containment check. It
	// is trust-gate-only: only catalog models resolve here, and a catalog model is
	// not risky, so there is no consent surface to show.
	m, ep, err := runner.gateAndServe(ctx, modelSpec, consentTrustGateOnly, 0, false)
	if err != nil {
		return nil, harness.Plan{}, fmt.Errorf("local model %s: %w", modelSpec, err)
	}
	plan := localModelPlan(ctx, m, dataDir)
	logPlan(os.Stderr, m.ID, plan)
	return localModelClient(ep, m.ID, plan, runner.httpClient()), plan, nil
}

// localModelPlan decides how much scaffolding a local model is driven with. Until an evaluation
// has measured this model, it has no profile, so it is treated as unknown and given the most
// conservative plan: the safe default is automatic, and a local model earns the lean path only by
// measuring as reliable, never on assumption. The advertised context window bounds the plan's
// context cap.
func localModelPlan(ctx context.Context, m catalog.ModelSpec, dataDir string) harness.Plan {
	return harness.PlanFor(localProfileSource(ctx, dataDir), m.ID, m.ContextTokens)
}

// localProfileSource loads the recorded model profiles from the durable store, so a local model's
// plan is driven by what `flynn models probe` measured rather than by assumption. It reads a
// snapshot and closes the store, so the lookup holds nothing open. A store that cannot be opened
// or read yields an empty source, which resolves every model as unknown: a profile lookup must
// only inform a run, never block it, so any failure falls back to the conservative default.
func localProfileSource(ctx context.Context, dataDir string) harness.ProfileSource {
	// No store on disk yet means nothing has been measured, so report every model as unknown
	// without creating a database: a read-only lookup must not have a side effect.
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
	reg, err := missionRegistry()
	if err != nil {
		return harness.StaticProfiles{}
	}
	src, err := profilestore.NewSource(ctx, store.Resources(reg))
	if err != nil {
		return harness.StaticProfiles{}
	}
	return src
}

// logPlan reports the scaffolding a model is driven with, so a run records why it was helped (or
// not). It is written to the progress stream, alongside the serve and provision lines.
func logPlan(w io.Writer, modelID string, plan harness.Plan) {
	_, _ = fmt.Fprintf(w, "harness plan for %s: constrain=%t simplify=%t verify=%d maxContext=%d\n",
		modelID, plan.ConstrainToolCalls, plan.SimplifyToolSchemas, plan.VerifyPasses, plan.MaxContext)
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

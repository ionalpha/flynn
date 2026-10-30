// Package agent is the Flynn open-source agent runtime.
//
// It is transport- and provider-agnostic, builds to a single static binary
// (see cmd/flynn), and exposes importable packages so a host application can
// embed it directly. Persistence is reached only through the interfaces in the
// state package, and observability through the observe package: the open agent ships
// local implementations and no-op defaults, while a richer host can supply its own,
// without this package depending on the host.
//
// A run is assembled by the governed package, the same assembly the flynn binary
// calls, so an embedded run carries the guards the binary does. [Config] adds backends
// and policy; it has no field whose zero value removes a guard, and the only way to
// run with less is a named [Policy.Waivers] entry, which is recorded on the run's
// spine stream.
package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ionalpha/flynn/archetypes"
	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/governed"
	"github.com/ionalpha/flynn/jobs"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/observe"
	"github.com/ionalpha/flynn/provider"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/secret"
	"github.com/ionalpha/flynn/spine"
	"github.com/ionalpha/flynn/state"
)

// DefaultSystemPrompt frames the agent for a coding or automation task. It is kept
// short on purpose: a capable model works better from a clear goal than from a long
// list of rules.
const DefaultSystemPrompt = `You are Flynn, an autonomous software agent working inside a sandboxed working directory.
You have tools to run shell commands and to read, write, edit, glob, and grep files; every command and file path is confined to the working directory.
Work toward the objective directly: inspect what you need, make the changes, and verify them with the tools rather than guessing.
When the objective is fully accomplished, stop and reply with a short summary of what you did.`

// Config configures an Agent. The zero value is usable: New fills in safe,
// standalone defaults so the agent always runs without external services. No field's
// zero value removes a guard; [Policy.Waivers] is the only opt-out.
type Config struct {
	// Model is the provider:model identifier (e.g. "anthropic:claude-opus-5-5").
	// Goal resolves it to a concrete model, reading the provider's API key from
	// Secrets, or the environment when Secrets is nil.
	Model string
	// Objective, when set, is the goal Run drives to completion. Programmatic
	// callers can pass the objective to Goal directly instead.
	Objective string
	// WorkDir is the sandbox root every tool is confined to. Defaults to the current
	// working directory.
	WorkDir string
	// Driver selects the run loop by name from the built-in registry (e.g.
	// "single-shot"). Empty uses the default general-purpose software loop, so a
	// zero-config agent is unchanged. An unknown name fails closed at run start.
	Driver string
	// Agent, when set, runs the goal as a specific Agent archetype: its system
	// prompt frames the run and its declared capabilities become the run's grant, so
	// a tool the archetype does not list is refused at the waist even though the host
	// offers it. Nil runs the default generalist (all tools granted). The model call
	// is always permitted.
	Agent *archetypes.Spec
	// State is the durable backend for sessions, skills, and memory. If nil, an
	// in-memory provider is used so the agent runs with zero setup.
	State state.Provider
	// Obs carries the logger and tracer. If nil, no-op defaults are used.
	Obs *observe.Observability
	// Out is where human-facing output is written. Defaults to io.Discard.
	Out io.Writer

	// Resources, Jobs and Log are the durable backends a run is reconciled and
	// recorded on: its goal and child goals, its work queue, and its spine stream. They
	// are set together or not at all; unset, the run uses in-process memory and is lost
	// when the process exits. Resources must have the kinds [governed.Registry]
	// registers. Setting some but not all is an error, because a run with a durable
	// store and a volatile record would look recorded and not be.
	Resources resource.Store
	Jobs      jobs.Queue
	Log       spine.Log
	// Sandbox, when set, confines every tool instead of a local sandbox rooted at
	// WorkDir. The run does not close it. It stays subject to the containment gate, and
	// the run's posture records that the host supplied it.
	Sandbox sandbox.Sandbox
	// Secrets resolves provider credentials, for the root model and for a delegated
	// child that names another. Nil reads the process environment.
	Secrets secret.Source
	// Policy carries what a host sets by list: its own tools, the actions that need
	// approval and the guards it waives. It is a pointer so Config stays comparable, and
	// nil asks for nothing beyond the governed defaults.
	Policy *Policy
	// Prompter asks a person to decide an approval. With no Policy.RequireApproval it
	// makes the run interactive without listing any action.
	Prompter mission.ApprovalPrompter
	// Reporter receives the run's conversational events in addition to the spine,
	// which always records them.
	Reporter mission.Reporter
	// Extensions adds the tools loaded extensions contribute, granted by name. A name
	// that collides with a tool the run already offers fails the run.
	Extensions governed.ToolBridge
	// ExternalAgent hands the loop to an external agent CLI; Model need not be set.
	// Combined with Policy.RequireApproval, Policy.Tools, Agent or Driver the run fails:
	// the external loop cannot enforce an approval policy or an archetype's capabilities
	// and is not given a toolset or a driver name.
	ExternalAgent *governed.ExternalAgent
}

// Policy is the list-valued part of [Config].
type Policy struct {
	// Tools, when set, replaces the sandboxed working-tree tools. The run then has no
	// sandbox to confine them, so it is refused unless Waivers names
	// [governed.GuardSandbox]. The grant is exactly these tools plus the model call,
	// narrowed by Config.Agent when that is set. Delegation is off in this mode.
	Tools []mission.Tool
	// RequireApproval lists the actions that pause for a person before they run. With
	// no Config.Prompter the listed actions are refused, because a run that cannot reach
	// a person does not proceed as though it had.
	RequireApproval []string
	// Waivers are the guards this run opts out of, each with the reason an auditor
	// will read. They are written to the run's stream before the run acts. Nothing
	// else in Config removes a guard.
	Waivers []governed.Waiver
}

// policy is the Policy to apply: the host's, or the empty one.
func (c Config) policy() Policy {
	if c.Policy == nil {
		return Policy{}
	}
	return *c.Policy
}

// Agent is the core runtime.
type Agent struct {
	cfg Config
}

// New constructs an Agent, filling in standalone defaults for any zero fields.
func New(cfg Config) *Agent {
	if cfg.State == nil {
		cfg.State = state.NewMemory()
	}
	if cfg.Obs == nil {
		cfg.Obs = observe.Default()
	}
	if cfg.Out == nil {
		cfg.Out = io.Discard
	}
	return &Agent{cfg: cfg}
}

// Run drives Config.Objective to completion and writes the model's final answer to
// Config.Out. It is the embedding entry point: a host sets a model and an objective
// and calls Run. With no objective configured it reports that nothing was given to
// do; programmatic callers that want a returned result call Goal instead.
func (a *Agent) Run(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	objective := strings.TrimSpace(a.cfg.Objective)
	if objective == "" {
		return errors.New("agent: no objective set; set Config.Objective or call Goal")
	}
	result, err := a.Goal(ctx, objective)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintln(a.cfg.Out, result)
	return nil
}

// Goal drives objective to completion and returns the model's final answer. It resolves
// the model from Config.Model, confines the tools to the working directory (unless
// Policy.Waivers says the host's own tools are not confined), and governs each model call
// and tool call through the dispatch waist. A caller that needs the run id, why it stopped or its record calls
// [Agent.GoalResult].
func (a *Agent) Goal(ctx context.Context, objective string) (string, error) {
	res, err := a.GoalResult(ctx, objective)
	return res.Text, err
}

// GoalResult is [Agent.Goal] reporting the whole outcome: the run id, why it stopped,
// the final text, the record and the posture the run was assembled with. The result is
// populated on a stall or cancellation as well as on success, so a failed run can
// still be found by its id.
func (a *Agent) GoalResult(ctx context.Context, objective string) (governed.Result, error) {
	var model llm.Model
	if a.cfg.ExternalAgent == nil {
		m, err := a.resolve(ctx, a.cfg.Model)
		if err != nil {
			return governed.Result{}, err
		}
		model = m
	}
	return a.runGoalResult(ctx, model, objective)
}

// resolve turns a provider:model spec into a model, reading credentials from
// Config.Secrets when the host supplied one.
func (a *Agent) resolve(ctx context.Context, spec string) (llm.Model, error) {
	if a.cfg.Secrets != nil {
		return provider.ResolveWith(ctx, spec, a.cfg.Secrets)
	}
	return provider.Resolve(spec)
}

// runGoal is [Agent.runGoalResult] reduced to the final text, for the tests that assert
// on the answer alone.
func (a *Agent) runGoal(ctx context.Context, model llm.Model, objective string, obs ...mission.Reporter) (string, error) {
	res, err := a.runGoalResult(ctx, model, objective, obs...)
	return res.Text, err
}

// runGoalResult assembles the governed run over model and drives one goal to a terminal
// result. The model is injected so the assembly is exercised end to end in tests with a
// scripted backend, without a network or an API key. An optional reporter lets a test
// observe the run's events through the same assembly it is meant to guard, rather than
// forking it; it takes the place of Config.Reporter.
func (a *Agent) runGoalResult(ctx context.Context, model llm.Model, objective string, obs ...mission.Reporter) (governed.Result, error) {
	objective = strings.TrimSpace(objective)
	if objective == "" {
		return governed.Result{}, errors.New("agent: empty objective")
	}
	b, err := a.backends()
	if err != nil {
		return governed.Result{}, err
	}
	opts, err := a.options(ctx, obs)
	if err != nil {
		return governed.Result{}, err
	}
	run, err := governed.Assemble(ctx, model, b, opts...)
	if err != nil {
		return governed.Result{}, err
	}
	defer func() { _ = run.Close() }()
	return run.Do(ctx, goal.Spec{Objective: objective})
}

// backends returns the host's durable backends, or in-process memory when it supplied
// none. A partial set is refused instead of filled in, so a run never mixes a durable
// store with a volatile record.
func (a *Agent) backends() (governed.Backends, error) {
	c := a.cfg
	set := 0
	for _, given := range []bool{c.Resources != nil, c.Jobs != nil, c.Log != nil} {
		if given {
			set++
		}
	}
	switch set {
	case 0:
		return governed.MemoryBackends()
	case 3:
		return governed.Backends{Resources: c.Resources, Jobs: c.Jobs, Log: c.Log}, nil
	}
	return governed.Backends{}, errors.New("agent: Config.Resources, Config.Jobs and Config.Log must be set together")
}

// options translates Config into the options of the governed assembly. Every guard is on
// unless Policy.Waivers names it.
func (a *Agent) options(ctx context.Context, obs []mission.Reporter) ([]governed.Option, error) {
	c := a.cfg
	workdir := c.WorkDir
	if workdir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		workdir = cwd
	}
	system := DefaultSystemPrompt
	if c.Agent != nil && c.Agent.System != "" {
		system = c.Agent.System
	}
	opts := []governed.Option{governed.WithWorkDir(workdir), governed.WithSystem(system)}
	if c.Sandbox != nil {
		opts = append(opts, governed.WithSandbox(c.Sandbox))
	}
	pol := c.policy()
	for _, w := range pol.Waivers {
		opts = append(opts, governed.Waive(w.Guard, w.Reason))
	}
	if len(pol.RequireApproval) > 0 || c.Prompter != nil {
		opts = append(opts, governed.WithApproval(pol.RequireApproval, c.Prompter))
	}
	reporter := c.Reporter
	if len(obs) > 0 && obs[0] != nil {
		reporter = obs[0]
	}
	if reporter != nil {
		opts = append(opts, governed.WithReporter(reporter))
	}
	if c.Extensions != nil {
		opts = append(opts, governed.WithExtensions(c.Extensions))
	}

	switch {
	case c.ExternalAgent != nil:
		// Each of these would be dropped silently and read as honoured: the external loop
		// brings its own tools and its own loop, so neither a grant nor a driver name binds it.
		switch {
		case len(pol.Tools) > 0:
			return nil, errors.New("agent: Policy.Tools does not apply to an external agent loop")
		case c.Agent != nil:
			return nil, errors.New("agent: Config.Agent does not apply to an external agent loop, which brings its own tools; its capabilities could not bound them")
		case c.Driver != "":
			return nil, errors.New("agent: Config.Driver does not apply to an external agent loop")
		}
		opts = append(opts, governed.WithExternalAgent(*c.ExternalAgent))
	case len(pol.Tools) > 0:
		// A toolset run has no router to select a loop, so a driver name would be dropped.
		if c.Driver != "" {
			return nil, errors.New("agent: Config.Driver does not apply to a run over Policy.Tools")
		}
		opts = append(opts, governed.WithToolset(pol.Tools, a.toolGrant()))
	default:
		// The default loop may also delegate to governed child runs: owned by the
		// parent, grant narrowed, depth- and concurrency-bounded.
		opts = append(opts, governed.WithFanout(func(id string) (llm.Model, error) { return a.resolve(ctx, id) }))
		if c.Driver != "" {
			opts = append(opts, governed.WithDriver(c.Driver))
		}
		// Running as an Agent narrows authority to exactly that archetype's declared
		// capabilities (plus the always-permitted model call), so the Agent resource is
		// the least-privilege boundary.
		if c.Agent != nil {
			opts = append(opts, governed.WithCapabilities(c.Agent.Capabilities...))
		}
	}
	return opts, nil
}

// toolGrant is the grant of a host-supplied toolset: each tool, plus the model call,
// kept to the Agent archetype's capabilities when one is set.
func (a *Agent) toolGrant() capability.Grant {
	names := []string{mission.ActionModelGenerate}
	allowed := map[string]bool{}
	if a.cfg.Agent != nil {
		for _, n := range a.cfg.Agent.Capabilities {
			allowed[n] = true
		}
	}
	for _, t := range a.cfg.policy().Tools {
		if n := t.Def().Name; a.cfg.Agent == nil || allowed[n] {
			names = append(names, n)
		}
	}
	return capability.NewGrant(names...)
}

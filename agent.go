// Package agent embeds the Flynn agent runtime in a host program; cmd/flynn
// builds the same runtime as a standalone binary.
//
// Persistence is reached only through the interfaces in the state package and
// observability through the observe package, so a host can supply its own
// backends without this package importing it. A run is assembled by the
// governed package, as the binary's runs are, so an embedded run carries the
// same guards.
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

// DefaultSystemPrompt frames the agent for a coding or automation task. It is
// short because a capable model works better from a clear goal than from a long
// list of rules.
const DefaultSystemPrompt = `You are Flynn, an autonomous software agent working inside a sandboxed working directory.
You have tools to run shell commands and to read, write, edit, glob, and grep files; every command and file path is confined to the working directory.
Work toward the objective directly: inspect what you need, make the changes, and verify them with the tools rather than guessing.
When the objective is fully accomplished, stop and reply with a short summary of what you did.`

// Config configures an Agent. The zero value runs with in-process defaults and
// no external services. No field's zero value removes a guard; [Policy.Waivers]
// is the only opt-out.
type Config struct {
	// Model is the provider:model identifier (e.g. "anthropic:claude-opus-5-5").
	// The provider's API key is read from Secrets, or the environment when Secrets
	// is nil.
	Model string
	// Objective is the goal Run drives. Callers of Goal pass it directly instead.
	Objective string
	// WorkDir is the sandbox root every tool is confined to. Defaults to the
	// current working directory.
	WorkDir string
	// Driver names the run loop in the built-in registry (e.g. "single-shot").
	// Empty selects the default software loop. An unknown name fails at run start.
	Driver string
	// Agent runs the goal as an archetype: its system prompt frames the run and its
	// declared capabilities become the grant, so a tool the archetype does not list
	// is refused even when the host offers it. Nil grants all tools. The model call
	// is always permitted.
	Agent *archetypes.Spec
	// State is the durable backend for sessions, skills and memory. Nil uses an
	// in-memory provider.
	State state.Provider
	// Obs carries the logger and tracer. Nil uses no-op defaults.
	Obs *observe.Observability
	// Out receives human-facing output. Defaults to io.Discard.
	Out io.Writer

	// Resources, Jobs and Log record the run's goals, work queue and spine stream.
	// Set all three or none; unset, the run is held in memory and lost when the
	// process exits. Resources must have the kinds [governed.Registry] registers. A
	// partial set is an error, because a durable store with a volatile record would
	// look recorded and not be.
	Resources resource.Store
	Jobs      jobs.Queue
	Log       spine.Log
	// Sandbox replaces the local sandbox rooted at WorkDir. The run does not close
	// it. It stays subject to the containment gate, and the run's posture records
	// that the host supplied it.
	Sandbox sandbox.Sandbox
	// Secrets resolves provider credentials for the root model and for a delegated
	// child that names another. Nil reads the process environment.
	Secrets secret.Source
	// Policy holds the list-valued settings. It is a pointer so Config stays
	// comparable; nil asks for nothing beyond the governed defaults.
	Policy *Policy
	// Prompter asks a person to decide an approval. Set without
	// Policy.RequireApproval, it makes the run interactive without listing any
	// action.
	Prompter mission.ApprovalPrompter
	// Reporter receives the run's conversational events. The spine records them
	// whether or not it is set.
	Reporter mission.Reporter
	// Extensions adds the tools loaded extensions contribute, granted by name. A
	// name that collides with a tool the run already offers fails the run.
	Extensions governed.ToolBridge
	// ExternalAgent hands the loop to an external agent CLI; Model need not be set.
	// The run fails when Policy.RequireApproval, Policy.Tools, Agent or Driver is
	// also set, because the external loop can enforce none of them.
	ExternalAgent *governed.ExternalAgent
}

// Policy is the list-valued part of [Config].
type Policy struct {
	// Tools replaces the sandboxed working-tree tools. Nothing then confines them,
	// so the run is refused unless Waivers names [governed.GuardSandbox]. The grant
	// is these tools plus the model call, narrowed by Config.Agent when set.
	// Delegation is off.
	Tools []mission.Tool
	// RequireApproval lists the actions that pause for a person. With no
	// Config.Prompter the listed actions are refused, because a run that cannot
	// reach a person must not proceed as though it had.
	RequireApproval []string
	// Waivers are the guards this run opts out of, each with the reason an auditor
	// reads. They are written to the run's stream before the run acts.
	Waivers []governed.Waiver
}

func (c Config) policy() Policy {
	if c.Policy == nil {
		return Policy{}
	}
	return *c.Policy
}

// Agent runs goals under a [Config].
type Agent struct {
	cfg Config
}

// New fills in defaults for the zero fields of cfg.
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

// Run drives Config.Objective and writes the final answer to Config.Out. With no
// objective it returns an error; callers that want the answer returned call Goal.
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

// Goal drives objective and returns the model's final answer. Tools are confined
// to WorkDir unless Policy.Waivers says otherwise, and every model and tool call
// goes through the dispatch waist. [Agent.GoalResult] also returns the run id,
// stop reason and record.
func (a *Agent) Goal(ctx context.Context, objective string) (string, error) {
	res, err := a.GoalResult(ctx, objective)
	return res.Text, err
}

// GoalResult is [Agent.Goal] with the whole outcome: run id, stop reason, final
// text, record and posture. The result is filled on a stall or cancellation too,
// so a failed run can be found by its id.
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

func (a *Agent) resolve(ctx context.Context, spec string) (llm.Model, error) {
	if a.cfg.Secrets != nil {
		return provider.ResolveWith(ctx, spec, a.cfg.Secrets)
	}
	return provider.Resolve(spec)
}

// runGoal is [Agent.runGoalResult] reduced to the final text, for tests.
func (a *Agent) runGoal(ctx context.Context, model llm.Model, objective string, obs ...mission.Reporter) (string, error) {
	res, err := a.runGoalResult(ctx, model, objective, obs...)
	return res.Text, err
}

// runGoalResult assembles the governed run over model and drives one goal. The
// model is a parameter so tests run the whole assembly against a scripted
// backend. A reporter in obs replaces Config.Reporter, so a test observes the
// same assembly it guards.
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

// backends returns the host's durable backends, or memory when it supplied
// none. A partial set is refused so a run never mixes a durable store with a
// volatile record.
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

// options maps Config onto the governed assembly's options. Every guard is on
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
		// The external loop brings its own tools and loop, so a grant or a driver name
		// would be dropped and look honoured.
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
		// A toolset run has no router, so a driver name would be dropped.
		if c.Driver != "" {
			return nil, errors.New("agent: Config.Driver does not apply to a run over Policy.Tools")
		}
		opts = append(opts, governed.WithToolset(pol.Tools, a.toolGrant()))
	default:
		// The default loop may delegate to child runs the parent owns, with a narrowed
		// grant and bounded depth and concurrency.
		opts = append(opts, governed.WithFanout(func(id string) (llm.Model, error) { return a.resolve(ctx, id) }))
		if c.Driver != "" {
			opts = append(opts, governed.WithDriver(c.Driver))
		}
		// An archetype narrows the grant to its declared capabilities plus the model
		// call.
		if c.Agent != nil {
			opts = append(opts, governed.WithCapabilities(c.Agent.Capabilities...))
		}
	}
	return opts, nil
}

// toolGrant grants each host tool plus the model call, narrowed to the
// archetype's capabilities when one is set.
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

package governed

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/ionalpha/flynn/brakes"
	budgetpkg "github.com/ionalpha/flynn/budget"
	"github.com/ionalpha/flynn/bus"
	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/driver"
	"github.com/ionalpha/flynn/evidence"
	"github.com/ionalpha/flynn/internal/spinesink"
	"github.com/ionalpha/flynn/learn"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/orchestration"
	"github.com/ionalpha/flynn/progress"
	"github.com/ionalpha/flynn/refusal"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/runtime"
	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/session"
	"github.com/ionalpha/flynn/spine"
	"github.com/ionalpha/flynn/tools"
)

// ToolBridge supplies the tools loaded extensions contribute. The extension loader
// satisfies it.
type ToolBridge interface {
	Tools() []mission.Tool
}

// parts are the ingredients every assembly shares: the sandbox, the session that records
// the run, the toolset, the grant that bounds every action, and the governance sink.
// Every mode builds from one of these, so the confinement, the tool surface, the
// recording and the grant are defined once and cannot drift between modes.
type parts struct {
	sb      sandbox.Sandbox
	owned   io.Closer
	sess    *session.Session
	toolset []mission.Tool
	grant   capability.Grant
	sink    *spinesink.Sink
}

func (p *parts) close() error {
	if p == nil || p.owned == nil {
		return nil
	}
	return p.owned.Close()
}

// newParts builds the shared ingredients. withSpawn adds the delegation action to the
// grant, so a fan-out may spawn sub-goals and a single conversation cannot.
func newParts(ctx context.Context, c *config, log spine.Log, withSpawn bool) (*parts, error) {
	var sopts []session.Option
	if c.runID != "" {
		sopts = append(sopts, session.WithID(c.runID))
	}
	sess := session.New(log, bus.NewMemory(), sopts...)
	p := &parts{sess: sess, sink: spinesink.New(log, sess.ID())}

	if c.hasToolset {
		// The caller's grant is the complete authority; there is nothing to derive it from.
		p.toolset, p.grant = c.toolset, c.toolGrant
		return p, nil
	}

	switch {
	case c.sandbox != nil:
		p.sb = c.sandbox
	default:
		workdir := c.workdir
		if workdir == "" {
			cwd, err := os.Getwd()
			if err != nil {
				return nil, err
			}
			workdir = cwd
		}
		// Secure by default: a model-authored command runs kernel-confined, with no network
		// and no view of the host's credential locations, unless the caller's policy says
		// otherwise. The policy is stated on the stream before anything runs in it, so it
		// precedes the session's opening event.
		sb, err := sandbox.NewLocal(workdir, c.shell.options()...)
		if err != nil {
			return nil, err
		}
		_, reason := c.shell.CredentialsReadable()
		if err := declareShellPosture(ctx, log, sess.ID(), sb, reason); err != nil {
			_ = sb.Close()
			return nil, fmt.Errorf("governed: record the shell's posture: %w", err)
		}
		p.sb, p.owned = sb, sb
	}

	// The working-tree tools, the skill tools when there is a store behind them, and the
	// tools loaded extensions contribute. They are one toolset from here on: the grant is
	// built from whatever this list holds, so a tool that is offered is a tool the waist
	// admits, and adding one cannot leave its authority behind.
	toolset := append(tools.New(p.sb).Tools(), c.skills.Tools()...)
	if c.bridge != nil {
		taken := map[string]bool{}
		for _, t := range toolset {
			taken[t.Def().Name] = true
		}
		for _, t := range c.bridge.Tools() {
			// An extension tool that reuses a built-in's name would inherit that name's
			// grant, so a third-party extension could stand in for `shell`.
			if name := t.Def().Name; taken[name] {
				_ = p.close()
				return nil, fmt.Errorf("governed: extension tool %q shadows a tool the run already offers", name)
			}
			toolset = append(toolset, t)
		}
	}
	p.toolset = toolset

	var names []string
	if c.hasCaps {
		// An archetype run is the least-privilege boundary: exactly what it declares, plus
		// the model call every run needs.
		names = append(append(names, c.caps...), mission.ActionModelGenerate)
	} else {
		for _, t := range toolset {
			names = append(names, t.Def().Name)
		}
		names = append(names, mission.ActionModelGenerate, learn.DistillAction, evidence.VerifyItemAction)
		if withSpawn {
			names = append(names, mission.ActionSpawn)
		}
	}
	p.grant = capability.NewGrant(names...)
	return p, nil
}

func (c *config) mode() (Mode, error) {
	n := 0
	mode := ModeConversation
	if c.hasToolset {
		n++
		mode = ModeToolset
	}
	if c.external != nil {
		n++
		mode = ModeExternal
	}
	if c.fanout != nil {
		n++
		mode = ModeFanout
	}
	if n > 1 {
		return "", errors.New("governed: WithToolset, WithExternalAgent and WithFanout are mutually exclusive")
	}
	return mode, nil
}

// checkWaiversAgainstOptions refuses a waiver for a guard the same option set also
// configures: the record would say the guard was given up while the run carried it, or the
// reverse, and a reader cannot tell which statement to believe.
func (c *config) checkWaiversAgainstOptions() error {
	if _, w := c.waivers[GuardApproval]; w && (len(ApprovalPolicy(c.approve)) > 0 || c.prompter != nil) {
		return errors.New("governed: Waive(GuardApproval) contradicts WithApproval")
	}
	if _, w := c.waivers[GuardBrakes]; w && c.brake != nil {
		return errors.New("governed: Waive(GuardBrakes) contradicts WithBrakeLimits")
	}
	if _, w := c.waivers[GuardBudget]; w && c.budget != nil {
		return errors.New("governed: Waive(GuardBudget) contradicts WithBudget")
	}
	return nil
}

// checkSandboxOptions rejects the combinations that claim a sandbox, or a shell policy over
// one, the mode does not have.
func (c *config) checkSandboxOptions(mode Mode) error {
	_, sandboxWaived := c.waivers[GuardSandbox]
	switch {
	case mode == ModeToolset && !sandboxWaived:
		return errors.New("governed: a toolset run has no sandbox; add Waive(GuardSandbox, reason) to say so")
	case mode != ModeToolset && sandboxWaived:
		return errors.New("governed: GuardSandbox can be waived only by a toolset run; every other mode builds or is given a sandbox")
	case mode == ModeToolset && c.sandbox != nil:
		return errors.New("governed: WithToolset and WithSandbox are mutually exclusive")
	case mode == ModeToolset && (c.hasCaps || c.bridge != nil):
		return errors.New("governed: a toolset run's grant is the caller's; WithCapabilities and WithExtensions do not apply")
	case c.shellSet && (mode == ModeToolset || c.sandbox != nil):
		return errors.New("governed: WithShellPolicy applies to the sandbox the run builds; a toolset run has no shell and WithSandbox brings the host's own")
	}
	return nil
}

// check rejects option combinations that would claim a guard the run does not have. It
// runs before anything is built so a contradiction leaves nothing to clean up.
func (c *config) check(mode Mode) error {
	if err := c.checkSandboxOptions(mode); err != nil {
		return err
	}
	if err := c.checkWaiversAgainstOptions(); err != nil {
		return err
	}
	if mode == ModeExternal && (len(ApprovalPolicy(c.approve)) > 0 || len(outsidePolicy(c.outside)) > 0) {
		return errors.New("governed: an external agent loop cannot enforce approval or allowance policy; run it natively or remove the policy")
	}
	if mode == ModeExternal && (c.hasCaps || c.bridge != nil) {
		return errors.New("governed: an external agent loop brings its own tools; WithCapabilities and WithExtensions could not bound them")
	}
	if c.driver != "" && mode != ModeFanout {
		return errors.New("governed: WithDriver selects the router's default loop and applies only to a fanout run")
	}
	return nil
}

// statePool makes the posture state the limits of the pool the run will be charged
// against. Opening returns a pool that already exists, as on a resume, so the limits asked
// for are not necessarily the ones in force, and the record states the latter.
func (r *Run) statePool(pool resource.Resource) error {
	spec, err := budgetpkg.DecodeSpec(pool)
	if err != nil {
		return err
	}
	r.posture.BudgetLimits = spec.Limits
	return nil
}

// Assemble composes one governed run over b. model drives the loop and may be nil only
// with [WithExternalAgent]. The spend ceiling from [WithBudget] is opened, each waiver is
// recorded as a [WaivedEventType] event and the posture as a [PostureEventType] event
// before it returns, so the ceiling and the record are in place ahead of the first action
// however the goal is later started: [Run.Submit], [Run.Resume], [Run.Do], or a caller that
// drives [Run.Runtime] itself. A resumed run is a new assembly and states its own.
// The caller owns the returned run for the length of one goal and closes it.
func Assemble(ctx context.Context, model llm.Model, b Backends, opts ...Option) (*Run, error) {
	if err := b.validate(); err != nil {
		return nil, err
	}
	c := &config{}
	for _, o := range opts {
		o(c)
	}
	if c.err != nil {
		return nil, c.err
	}
	mode, err := c.mode()
	if err != nil {
		return nil, err
	}
	if model == nil && mode != ModeExternal {
		return nil, errors.New("governed: a model is required unless the loop is an external agent")
	}
	if err := c.check(mode); err != nil {
		return nil, err
	}

	p, err := newParts(ctx, c, b.Log, mode == ModeFanout)
	if err != nil {
		return nil, err
	}
	if err := c.checkActionNames(p.grant); err != nil {
		_ = p.close()
		return nil, err
	}
	run, err := c.build(model, b, p, mode)
	if err != nil {
		_ = p.close()
		return nil, err
	}
	// Done last so a build that failed leaves no ceiling or waiver behind for a run that
	// never existed, and before the caller can start the run, so the ceiling is in force
	// and the posture is on the record ahead of its first action on every path that starts
	// one. The waivers go first: they say what was given up, the posture what remains.
	if c.budget != nil && run.posture.Budget {
		pool, err := budgetpkg.NewLedger(b.Resources).Open(ctx, p.sess.ID(), resource.Scope{}, *c.budget)
		if err == nil {
			err = run.statePool(pool)
		}
		if err != nil {
			_ = p.close()
			return nil, fmt.Errorf("governed: open run budget: %w", err)
		}
	}
	if err := recordWaivers(ctx, b.Log, p.sess.ID(), c.waiveList); err != nil {
		_ = p.close()
		return nil, fmt.Errorf("governed: record waivers: %w", err)
	}
	if err := recordPosture(ctx, b.Log, p.sess.ID(), run.posture); err != nil {
		_ = p.close()
		return nil, fmt.Errorf("governed: record posture: %w", err)
	}
	return run, nil
}

// stack and gate state shared by the builders below.
type gates struct {
	stack *approvalStack
	brk   *brakes.Hook
	pool  *budgetpkg.Hook
}

func (c *config) gates(b Backends, p *parts) (gates, error) {
	var g gates
	if _, waived := c.waivers[GuardApproval]; !waived {
		// Built against the session's own id, which is the stream its authorization
		// decisions are recorded on, so a run's approvals are sealed with the rest of what
		// it did rather than landing in a log beside it.
		stack, err := newApprovalStack(c.approve, b.Log, p.sess.ID())
		if err != nil {
			return g, err
		}
		g.stack = stack
	}
	if _, waived := c.waivers[GuardBrakes]; !waived {
		// Held rather than built inline because two things are wired from it: the waist the
		// run dispatches through, and the halt an operator's kill engages. They have to be
		// the same brake; a kill bound to a second one would engage a halt nothing on the
		// run's path consults.
		g.brk = c.newBrakes()
	}
	if _, waived := c.waivers[GuardBudget]; !waived {
		g.pool = budgetpkg.NewHook(b.Resources)
	}
	return g, nil
}

// brakeLimits is the limits the run's brakes are built with, so the posture states the
// ones in force and not the ones asked for.
func (c *config) brakeLimits() brakes.Limits {
	if c.brake != nil {
		return *c.brake
	}
	return defaultBrakeLimits()
}

func defaultBrakeLimits() brakes.Limits {
	return brakes.Limits{MaxActions: DefaultMaxActionsPerMinute, Window: time.Minute}
}

func (c *config) newBrakes() *brakes.Hook { return brakes.NewHook(c.brakeLimits(), nil) }

// DefaultBrakes builds the run's safety governor: the circuit breaker that halts a
// runaway from outside the model loop. Its rate is a generous backstop that fires only on
// a degenerate tight loop, never on legitimate tool use. Each call returns a fresh Hook
// with its own in-memory kill-switch, so a run's breaker state and halt are its own.
func DefaultBrakes() *brakes.Hook { return brakes.NewHook(defaultBrakeLimits(), nil) }

func (c *config) observer(p *parts) mission.Reporter {
	if c.reporter == nil {
		return p.sess.Reporter()
	}
	return tee{p.sess.Reporter(), c.reporter}
}

func (c *config) build(model llm.Model, b Backends, p *parts, mode Mode) (*Run, error) {
	g, err := c.gates(b, p)
	if err != nil {
		return nil, err
	}
	post := Posture{
		Mode:        mode,
		Sandbox:     p.sb != nil,
		HostSandbox: c.sandbox != nil,
		Approval:    g.stack != nil,
		Allowance:   len(outsidePolicy(c.outside)) > 0,
		Brakes:      g.brk != nil,
		Budget:      g.pool != nil,
		EventSink:   true,
		Waived:      map[Guard]string{},
	}
	if g.stack != nil {
		post.ApprovalActions = g.stack.actions
	}
	if mode == ModeExternal {
		// An external agent loop runs its own tools beyond the approval gate, so the gate is
		// built and never consulted. Claiming it would tell a reader of the record that a
		// human could stop an action nothing routes through one.
		post.Approval, post.ApprovalActions = false, nil
	}
	if g.pool != nil && c.budget != nil {
		post.BudgetLimits = *c.budget
	}
	if g.brk != nil {
		post.BrakeLimits = c.brakeLimits()
	}
	for k, v := range c.waivers {
		post.Waived[k] = v
	}
	_, auditWaived := c.waivers[GuardAudit]

	cfg := runtime.Config{
		Store:              b.Resources,
		Jobs:               b.Jobs,
		PollInterval:       200 * time.Millisecond,
		WorkerPoll:         50 * time.Millisecond,
		DriveSubmittedOnly: true,
	}
	// Never assigned from a nil hook: Halt is an interface, and a typed nil would read as
	// a halt that is wired.
	if g.brk != nil {
		cfg.Halt = g.brk
		post.Halt = true
	}

	var spawner *orchestration.Spawner
	switch mode {
	case ModeExternal:
		RecordAttested(c.external.Driver, b.Log, p.sess.ID())
		spec := driver.Spec{
			Tools: p.toolset, System: c.system, Grant: p.grant, HasGrant: true,
			Sandbox: p.sb, Reporter: c.observer(p), EventSink: p.sink, Brakes: g.brk, Budget: g.pool,
		}
		exec, stop, err := c.external.Driver.Build(spec)
		if err != nil {
			return nil, err
		}
		cfg.Executor, cfg.Stop = exec, stop
	case ModeFanout:
		spawner = orchestration.NewSpawner(b.Resources, nil, orchestration.WithConcurrency(DefaultFanoutWidth))
		base := driver.Spec{
			Tools: p.toolset, System: c.system, Grant: p.grant, HasGrant: true,
			Sandbox: p.sb, Reporter: c.observer(p), Fanout: spawner, EventSink: p.sink,
			CompactionBudget: DefaultCompactionBudget, Brakes: g.brk, Budget: g.pool,
			Approval: g.stack.spec(c.prompter), Allowance: outsideGate(c.outside), Plan: c.plan,
		}
		router := driver.NewRouter(driver.RouterConfig{
			Registry: driver.Default(), DefaultModel: model, DefaultDriver: c.driver,
			ResolveModel: c.fanout.resolve, Base: base,
		})
		cfg.Executor, cfg.Stop = router, router
		var hooks []dispatch.Hook
		if g.brk != nil {
			hooks = append(hooks, g.brk)
		}
		if g.pool != nil {
			hooks = append(hooks, g.pool)
		}
		cfg.Units = orchestration.Units(spawner, orchestration.UnitGovernor(p.sb, hooks...))
		// A unit is settled from its child's ledger, so the evidence loop has to be closed
		// or every unit fails as unproven: the child carries the unit's verify clause as a
		// ledger item and nothing else would run it. The ledger, not the model's final
		// answer, decides a goal that carries one.
		cfg.Verifier = evidence.NewCommandVerifier(p.sb, contained(p.sb)...)
		cfg.CheckDir = c.checkDir()
		cfg.Evidence = evidence.NewSpineEvidence(b.Log)
		cfg.RequireLedgerProof = true
		post.Verifier, post.Evidence, post.LedgerProof = true, true, true
		c.judges(&cfg, &post, model, b.Log, p, true, auditWaived)
	case ModeToolset:
		cfg.Executor, cfg.Stop = c.executor(model, b, p, g), mission.Convergence{}
		if c.planning {
			cfg.Planner = mission.NewPlanner(model)
		}
	default:
		cfg.Executor, cfg.Stop = c.executor(model, b, p, g), mission.Convergence{}
		if c.planning {
			cfg.Planner = mission.NewPlanner(model)
			// The item's declared check is model-authored, so it runs in the run's own
			// sandbox under the same containment gate as the agent's shell tool.
			//
			// It deliberately does not take the governance event sink, and neither does the
			// auditor: a dispatcher's correlation ids are monotonic within that dispatcher,
			// so two dispatchers writing lifecycle events to one stream emit colliding call
			// ids, and the record then reads as one call both refused and completed. The
			// verdict is on the record as the item-verified event the gate consumes.
			cfg.Verifier = evidence.NewCommandVerifier(p.sb, contained(p.sb)...)
			cfg.CheckDir = c.checkDir()
			cfg.Evidence = evidence.NewSpineEvidence(b.Log)
			cfg.RequireLedgerProof = c.ledgerProof
			post.Verifier, post.Evidence, post.LedgerProof = true, true, c.ledgerProof
		}
		c.judges(&cfg, &post, model, b.Log, p, false, auditWaived)
	}
	post.Containment = p.sb != nil

	rt, err := runtime.New(cfg)
	if err != nil {
		return nil, err
	}
	if spawner != nil {
		// Bound after the runtime exists: the executor holds the spawner and the runtime
		// holds the executor, so binding at construction would be a cycle.
		spawner.SetEnqueue(func(ctx context.Context, key resource.Key) error {
			_, rerr := rt.Resume(ctx, key.Name)
			return rerr
		})
	}
	return &Run{
		rt: rt, p: p, store: b.Resources, log: b.Log, posture: post,
		mode: mode, external: c.external,
	}, nil
}

// contained is the dispatch options the run-level checkers share: the capability
// admitter and the containment gate, so a model-authored clause is treated as untrusted
// work.
func contained(sb sandbox.Sandbox) []dispatch.Option {
	return []dispatch.Option{
		dispatch.WithAdmitter(capability.Admitter{}),
		dispatch.WithHook(capability.NewContainmentGate(sb)),
	}
}

// executor builds the native mission executor for the conversation and toolset modes.
func (c *config) executor(model llm.Model, b Backends, p *parts, g gates) *mission.Executor {
	opts := []mission.Option{
		mission.WithTools(p.toolset...),
		mission.WithSystem(c.system),
		mission.WithObserver(c.observer(p)),
		mission.WithGrant(p.grant),
		mission.WithEventSink(p.sink),
		mission.WithCompactionBudget(DefaultCompactionBudget),
	}
	if p.sb != nil {
		// Gate every action on containment sufficiency, as the router does for the other
		// modes: a work kind whose trust needs stronger isolation than the sandbox gives is
		// refused rather than downgraded.
		opts = append(opts, mission.WithSandbox(p.sb))
	}
	if g.pool != nil {
		opts = append(opts, mission.WithBudget(g.pool))
	}
	if g.brk != nil {
		opts = append(opts, mission.WithBrakes(g.brk))
	}
	opts = append(opts, g.stack.options(c.prompter)...)
	opts = append(opts, allowanceOptions(c.outside)...)
	// The model's scaffolding plan goes last so a present field overrides the lean
	// defaults while an absent one leaves them in place.
	opts = append(opts, mission.PlanOptions(c.plan)...)
	return mission.NewExecutor(model, opts...)
}

// judges wires the run-level judges onto cfg: the invariant auditor, the steer judge,
// and for a conversation the progress and refusal probes. They need the run's sandbox,
// so a mode without one has none, and a waived [GuardAudit] drops all of them.
func (c *config) judges(cfg *runtime.Config, post *Posture, model llm.Model, log spine.Log, p *parts, fanout, waived bool) {
	if waived || p.sb == nil {
		return
	}
	// A term that declares no check goes to the model auditor, which reads the run's own
	// recorded events and refuses to rule on a claim that something is absent or cite
	// nothing. Both refusals stop the goal, the same fail-closed direction as an
	// unrunnable check. The event sink is left off for the reason the verifier leaves it
	// off.
	cfg.Auditor = evidence.NewCommandAuditor(p.sb, log, evidence.NewModelAuditor(model, log), contained(p.sb)...)
	cfg.SteerJudge = evidence.NewModelSteerJudge(model, log)
	post.Auditor, post.SteerJudge = true, true
	if fanout {
		return
	}
	cfg.Progress = progress.NewSpineProbe(log, c.progressDir())
	cfg.Refusals = refusal.NewSpineProbe(log)
	post.Progress, post.Refusals = true, true
}

// checkDir is the directory a ledger check runs in, which the worker searches for a file
// standing in for the check's command. It is the run's working directory when governed
// built the sandbox, and empty when the host supplied one, because a host sandbox has no
// path the run chose and the process's directory would refuse checks for files the check
// never sees.
func (c *config) checkDir() string {
	if c.sandbox != nil {
		return ""
	}
	return c.progressDir()
}

// progressDir is the directory the progress probe reads git HEAD from. A host sandbox
// has no path the run chose, so the probe watches the working directory the run was
// given or the process's.
func (c *config) progressDir() string {
	if c.workdir != "" {
		return c.workdir
	}
	cwd, _ := os.Getwd()
	return cwd
}

package governed

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
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

// ToolBridge supplies the tools loaded extensions contribute.
type ToolBridge interface {
	Tools() []mission.Tool
}

// parts are what every mode builds from, so confinement, tools, recording and grant
// cannot drift between modes.
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
		// The caller's grant is the whole authority.
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
		// By default a command runs kernel-confined with no network and no view of host
		// credentials. The policy is recorded before the session's opening event.
		sb, err := sandbox.NewLocal(workdir, c.shell.options()...)
		if err != nil {
			return nil, err
		}
		_, reason := c.shell.CredentialsReadable()
		_, networkReason := c.shell.NetworkOpen()
		if err := declareShellPosture(ctx, log, sess.ID(), sb, reason, networkReason); err != nil {
			_ = sb.Close()
			return nil, fmt.Errorf("governed: record the shell's posture: %w", err)
		}
		p.sb, p.owned = sb, sb
	}

	// The grant is built from this list, so every offered tool is admitted and none is
	// admitted without being offered.
	toolset := append(tools.New(p.sb).Tools(), c.skills.Tools()...)
	if c.bridge != nil {
		taken := map[string]bool{}
		for _, t := range toolset {
			taken[t.Def().Name] = true
		}
		for _, t := range c.bridge.Tools() {
			// A reused name would inherit the built-in's grant, so an extension could pose
			// as `shell`.
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
		// An archetype run gets exactly what it declares, plus the model call.
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

// checkWaiversAgainstOptions refuses waiving a guard the options also configure, because
// the record would then contradict itself.
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

// checkSandboxOptions rejects claiming a sandbox, or a shell policy, the mode lacks.
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

// check rejects options claiming a guard the run lacks. It runs before anything is built,
// so a contradiction leaves nothing to clean up.
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

// statePool records the opened pool's limits, because on a resume Open returns the
// existing pool and its limits may differ from those asked for.
func (r *Run) statePool(pool resource.Resource) error {
	spec, err := budgetpkg.DecodeSpec(pool)
	if err != nil {
		return err
	}
	r.posture.BudgetLimits = spec.Limits
	return nil
}

// Assemble composes one governed run over b. model may be nil only with
// [WithExternalAgent]. Before it returns, the [WithBudget] ceiling is opened and waivers
// ([WaivedEventType]) and posture ([PostureEventType]) are recorded, so they precede the
// first action however the goal starts, including through [Run.Runtime]. The caller owns
// the run for one goal and closes it.
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
	// After build, so a failed build leaves no ceiling or waiver behind. Waivers precede
	// the posture: what was given up, then what remains.
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

type gates struct {
	stack *approvalStack
	brk   *brakes.Hook
	pool  *budgetpkg.Hook
}

func (c *config) gates(b Backends, p *parts) (gates, error) {
	var g gates
	if _, waived := c.waivers[GuardApproval]; !waived {
		// Approvals are recorded on the session's own stream, sealed with the rest of the
		// run.
		stack, err := newApprovalStack(c.approve, b.Log, p.sess.ID())
		if err != nil {
			return g, err
		}
		g.stack = stack
	}
	if _, waived := c.waivers[GuardBrakes]; !waived {
		// One brake serves dispatch and the operator's kill, because a kill bound to a
		// second brake would halt nothing the run consults.
		g.brk = c.newBrakes()
	}
	if _, waived := c.waivers[GuardBudget]; !waived {
		g.pool = budgetpkg.NewHook(b.Resources)
	}
	return g, nil
}

// brakeLimits is shared by the brakes and the posture, so the posture states the limits
// in force.
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

// DefaultBrakes returns a brake at [DefaultMaxActionsPerMinute]. Each call has its own
// in-memory kill-switch, so runs do not share breaker state.
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
	if outside := outsidePolicy(c.outside); len(outside) > 0 {
		post.AllowanceActions = slices.Sorted(maps.Keys(outside))
	}
	if mode == ModeExternal {
		// An external loop's tools bypass the approval gate, so the posture must not claim
		// it.
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
	// Halt is an interface, so a typed nil would read as wired.
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
			Approval: g.stack.spec(g.stack.recording(c.prompter)), Allowance: outsideGate(c.outside), Plan: c.plan,
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
		// A unit settles from its child's ledger, and only this verifier runs the unit's
		// verify clause, so without it every unit fails as unproven.
		cfg.Verifier = evidence.NewCommandVerifier(p.sb, contained(p.sb)...)
		cfg.CheckDir = c.checkDir()
		cfg.Evidence = evidence.NewSpineEvidence(b.Log)
		cfg.RequireLedgerProof = true
		post.Verifier, post.Evidence, post.LedgerProof = true, true, true
		c.judges(&cfg, &post, model, b.Log, p, true, auditWaived)
	case ModeToolset:
		cfg.Executor, cfg.Stop = c.executor(model, b, p, g), mission.Convergence{}
		if c.planning {
			cfg.Planner = planner(model, p.toolset)
		}
	default:
		cfg.Executor, cfg.Stop = c.executor(model, b, p, g), mission.Convergence{}
		if c.planning {
			// A model-authored check runs in the run's sandbox under the shell tool's
			// containment gate. The verifier and auditor take no event sink, because two
			// dispatchers on one stream emit colliding call ids. The verdict is recorded as
			// the item-verified event.
			cfg.Verifier = evidence.NewCommandVerifier(p.sb, contained(p.sb)...)
			cfg.CheckDir = c.checkDir()
			cfg.Evidence = evidence.NewSpineEvidence(b.Log)
			// The planner admits each check with the verifier that will run it.
			cfg.Planner = planner(model, p.toolset, mission.WithPlannerAdmission(mission.Admission{
				Verifier: cfg.Verifier, Recorder: evidence.NewSpineEvidence(b.Log), Dir: cfg.CheckDir, Confined: p.sb != nil,
			}))
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
		// Bound late, because runtime holds executor holds spawner.
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

// contained treats a model-authored check as untrusted work.
func contained(sb sandbox.Sandbox) []dispatch.Option {
	return []dispatch.Option{
		dispatch.WithAdmitter(capability.Admitter{}),
		dispatch.WithHook(capability.NewContainmentGate(sb)),
	}
}

// planner is told the run's tool names, so a plan whose check calls a tool instead of a
// shell command is sent back before it stalls the goal.
func planner(model llm.Model, toolset []mission.Tool, extra ...mission.PlannerOption) *mission.Planner {
	names := make([]string, 0, len(toolset))
	for _, t := range toolset {
		names = append(names, t.Def().Name)
	}
	return mission.NewPlanner(model, append([]mission.PlannerOption{mission.WithPlannerTools(names...)}, extra...)...)
}

// executor serves the conversation and toolset modes.
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
		// A work kind needing more isolation than the sandbox gives is refused.
		opts = append(opts, mission.WithSandbox(p.sb))
	}
	if g.pool != nil {
		opts = append(opts, mission.WithBudget(g.pool))
	}
	if g.brk != nil {
		opts = append(opts, mission.WithBrakes(g.brk))
	}
	opts = append(opts, g.stack.options(g.stack.recording(c.prompter))...)
	opts = append(opts, allowanceOptions(c.outside)...)
	// The model plan goes last, so a set field overrides the defaults.
	opts = append(opts, mission.PlanOptions(c.plan)...)
	return mission.NewExecutor(model, opts...)
}

// judges needs the run's sandbox, so a mode without one gets no judges, and a waived
// [GuardAudit] drops them all. Only a conversation gets the progress and refusal probes.
func (c *config) judges(cfg *runtime.Config, post *Posture, model llm.Model, log spine.Log, p *parts, fanout, waived bool) {
	if waived || p.sb == nil {
		return
	}
	// A term with no check goes to the model auditor. No event sink, as for the verifier.
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

// checkDir is where a ledger check runs. It is empty for a host sandbox, which has no
// path the run chose, so the process directory cannot refuse checks for files it never
// sees.
func (c *config) checkDir() string {
	if c.sandbox != nil {
		return ""
	}
	return c.progressDir()
}

// progressDir is where the progress probe reads git HEAD: the given workdir, or the
// process's.
func (c *config) progressDir() string {
	if c.workdir != "" {
		return c.workdir
	}
	cwd, _ := os.Getwd()
	return cwd
}

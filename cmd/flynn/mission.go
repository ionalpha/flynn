package main

import (
	"time"

	budgetpkg "github.com/ionalpha/flynn/budget"
	"github.com/ionalpha/flynn/bus"
	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/driver"
	"github.com/ionalpha/flynn/evidence"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/harness"
	"github.com/ionalpha/flynn/internal/spinesink"
	"github.com/ionalpha/flynn/jobs"
	"github.com/ionalpha/flynn/learn"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/progress"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/runtime"
	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/session"
	"github.com/ionalpha/flynn/skill/skilltool"
	"github.com/ionalpha/flynn/spine"
	"github.com/ionalpha/flynn/state"
	"github.com/ionalpha/flynn/tools"
)

// missionRun is an assembled goal runtime paired with the session that records it
// onto the spine. The caller starts rt (rt.Start in a goroutine), then submits or
// resumes a goal through sess; cancelling the context stops the control plane.
type missionRun struct {
	rt   *runtime.Runtime
	sess *session.Session
	// parts holds the ingredients whose lifetime is the run's, so closing the run closes
	// them. The sandbox among them registers operating-system objects that outlive the
	// process unless they are released.
	parts *missionParts
}

// Close releases the run's sandbox. A caller owns a missionRun for the length of one run
// (a goal, a resumed run, an interactive session, a served conversation) and closes it
// when that run ends.
func (m *missionRun) Close() error {
	if m == nil {
		return nil
	}
	return m.parts.Close()
}

// missionParts is the set of ingredients every one-shot runtime assembly shares:
// the confined sandbox, the session that records the run onto the spine, the
// sandboxed toolset, the capability grant that bounds every action the run may take,
// and the governance event sink. Both the single-conversation runner and the fan-out
// runner build from one of these, so the sandbox confinement, tool surface,
// governance recording, and (the reason this is one function) the grant that decides
// what a run is allowed to do are defined once and cannot drift between the two paths.
type missionParts struct {
	sandbox *sandbox.Local
	sess    *session.Session
	toolset []mission.Tool
	grant   capability.Grant
	sink    *spinesink.Sink
}

// Close releases the run's sandbox, which is what hands back the operating-system objects
// its confinement registered (on Windows, the container profile whose identity outlives
// the process that made it). Every caller of newMissionParts owns the parts for the length
// of a run and closes them when it ends.
func (p *missionParts) Close() error {
	if p == nil || p.sandbox == nil {
		return nil
	}
	return p.sandbox.Close()
}

// newMissionParts wires the shared ingredients for a run at workdir recording onto
// log under runID (empty gets a fresh one). withSpawn adds the delegation action to
// the grant, so a fan-out run may spawn sub-goals and a single conversation cannot;
// everything else is identical across the two paths. resLimits caps the host memory
// and process count of the commands the run's tools execute (its zero value applies
// no cap).
//
// skills is the durable skill store the skill tools read, and may be nil: a run
// assembled without one (a served control-plane run, a test) simply offers no skill
// tool, rather than offering one that answers nothing.
func newMissionParts(workdir string, log spine.Log, skills state.SkillStore, runID string, withSpawn bool, resLimits sandbox.ResourceLimits) (*missionParts, error) {
	sb, err := sandbox.NewLocal(workdir, sandbox.WithDefaultConfinement(), sandbox.WithResourceLimits(resLimits))
	if err != nil {
		return nil, err
	}

	var sopts []session.Option
	if runID != "" {
		sopts = append(sopts, session.WithID(runID))
	}
	sess := session.New(log, bus.NewMemory(), sopts...)

	// The working-tree tools, plus the skill tools when there is a store behind them.
	// They are one toolset from here on: the grant below is built from whatever this
	// list holds, so a tool that is offered is a tool the waist admits, and adding one
	// cannot leave its authority behind.
	toolset := append(tools.New(sb).Tools(), skilltool.New(skills).Tools()...)
	// The grant lists every action the run may take: the tools, plus the model call
	// and the distillation, and (for a fan-out) the spawn that delegates a sub-goal. A
	// child narrows from this set, so a delegation can never widen authority; a run
	// whose grant omitted spawn could not fan out at all. Assembling the action set in
	// one place is the point: the single and fan-out paths cannot grant different
	// authority by drift.
	names := make([]string, 0, len(toolset)+3)
	for _, t := range toolset {
		names = append(names, t.Def().Name)
	}
	names = append(names, mission.ActionModelGenerate, learn.DistillAction, evidence.VerifyItemAction)
	if withSpawn {
		names = append(names, mission.ActionSpawn)
	}

	return &missionParts{
		sandbox: sb,
		sess:    sess,
		toolset: toolset,
		grant:   capability.NewGrant(names...),
		sink:    spinesink.New(log, sess.ID()),
	}, nil
}

// runtimeConfig returns the runtime.Config both assemblies share, with the caller's
// executor and stop condition dropped in. Every one-shot CLI run polls at the same
// cadence and drives only its own submitted goal (and, for a fan-out, the children
// it spawns) - never a parked goal an earlier run left non-terminal, which would
// contaminate this run's stream and silently resume unrelated work. Resuming a parked
// run, or continuing a session turn, is always explicit.
func (p *missionParts) runtimeConfig(exec goal.StepExecutor, stop goal.StopEvaluator, rstore resource.Store, jq jobs.Queue) runtime.Config {
	return runtime.Config{
		Executor:           exec,
		Stop:               stop,
		Store:              rstore,
		Jobs:               jq,
		PollInterval:       200 * time.Millisecond,
		WorkerPoll:         50 * time.Millisecond,
		DriveSubmittedOnly: true,
	}
}

// assembleMission wires one goal runtime over the durable store ports and the
// sandboxed toolset at workdir, with a session recording the run onto the spine.
// runID names both the session's event stream and (via Submit/Resume) its goal
// resource, so a single id addresses the whole run for replay, audit, and resume; an
// empty runID gets a fresh one. The system prompt is supplied so the caller can fold
// recalled knowledge into it. It is the shared assembly behind the one-shot runner,
// resume, and the interactive session, so none of them reassembles the runtime by
// hand.
func assembleMission(model llm.Model, plan harness.Plan, workdir, system string, rstore resource.Store, jq jobs.Queue, log spine.Log, skills state.SkillStore, runID string, resLimits sandbox.ResourceLimits, planning, requireProof bool) (*missionRun, error) {
	parts, err := newMissionParts(workdir, log, skills, runID, false, resLimits)
	if err != nil {
		return nil, err
	}

	opts := []mission.Option{
		mission.WithTools(parts.toolset...),
		mission.WithSystem(system),
		mission.WithObserver(parts.sess.Reporter()),
		mission.WithGrant(parts.grant),
		// Charge every action against the run's spend pool, so a ceiling set for the run
		// (flynn run --max-cost/--max-tokens) halts it once reached. It is inert until a
		// budget is opened for the run: a pool with no budget resource is unlimited, so a
		// run without a ceiling is unchanged, and a resumed run honours the durable budget
		// its first run opened.
		mission.WithBudget(budgetpkg.NewHook(rstore)),
		// Halt a runaway from outside the model loop: the same circuit breaker the
		// fan-out runs under, so a single conversation cannot spin unbounded (a jailbroken
		// or looping model hammering a tool) any more than a delegating one can. The rate
		// is a generous backstop that never trips on legitimate use.
		mission.WithBrakes(defaultBrakes()),
		// Record every governed action's lifecycle (admitted, completed, or rejected)
		// onto the run's own stream, so the admission decisions are part of the run's
		// recorded and sealed history rather than only the live trace. The stream is the
		// session's, so governance events interleave with the run's other events in one
		// ordered log.
		mission.WithEventSink(parts.sink),
		// Compact the transcript when it grows past this budget so a long session
		// stays affordable and clear of the context limit. It is a conservative floor
		// for a model whose window is unknown; the plan below tightens it to a model
		// with a measured, narrower effective context.
		mission.WithCompactionBudget(defaultCompactionBudget),
	}
	// Apply the model's scaffolding plan last so a present field (a tighter context
	// budget, simplified schemas, verify passes) overrides the lean defaults, while an
	// absent one (the zero plan of a strong model) leaves them in place.
	opts = append(opts, mission.PlanOptions(plan)...)
	exec := mission.NewExecutor(model, opts...)
	cfg := parts.runtimeConfig(exec, mission.Convergence{}, rstore, jq)
	// Plan before building when the entry point asked for it: expand the objective into a
	// visible ledger on the goal before the first build step. The planner runs its own
	// prompt over the same model; the runtime pairs it with the reconciler's planning gate.
	if planning {
		cfg.Planner = mission.NewPlanner(model)
		// Close the loop the ledger opens. Without this the plan is written, validated and
		// protected against tampering, and then never asked whether the work was actually
		// done: the exact failure a ledger exists to foreclose. Each item's declared check
		// is run in the run's own sandbox, under the same containment gate as the agent's
		// shell tool (the clause is model-authored, so it is treated as untrusted work), and
		// the verdict is recorded on the run's own stream where the evidence gate reads it.
		//
		// It deliberately does NOT take the governance event sink, and neither does the
		// learning loop's verifier beside it. A dispatcher's correlation ids are monotonic
		// within that dispatcher, so two dispatchers writing lifecycle events to one stream
		// emit colliding call ids, and the record then reads as one call both refused and
		// completed: a governance violation invented by the wiring. The verification is on
		// the record where it belongs, as the item-verified event the gate consumes.
		cfg.Verifier = evidence.NewCommandVerifier(parts.sandbox,
			dispatch.WithAdmitter(capability.Admitter{}),
			dispatch.WithHook(capability.NewContainmentGate(parts.sandbox)))
		cfg.Evidence = evidence.NewSpineEvidence(log)
		cfg.RequireLedgerProof = requireProof
	}
	// Rule on the terms of the run: after each step, every invariant the goal states has
	// its declared check run in the same sandbox, and a breach stops the goal before its
	// stop condition is evaluated. It is wired outside the planning branch because a term
	// is not part of the ledger: a goal can state what must stay true without expanding
	// its objective into items, and one that states terms with no auditor would stall.
	//
	// The event sink is left off for the reason the item verifier leaves it off: a second
	// dispatcher writing lifecycle events onto one stream emits colliding call ids, and
	// the record then reads as one call both refused and completed. The audit is on the
	// record as its own invariant-audited event, which is the part that matters.
	cfg.Auditor = evidence.NewCommandAuditor(parts.sandbox, log,
		dispatch.WithAdmitter(capability.Admitter{}),
		dispatch.WithHook(capability.NewContainmentGate(parts.sandbox)))
	// Stop a run that has stopped getting anywhere. The probe reads the run's own recorded
	// activity (its stream on the spine) and the git HEAD at the working directory, so a
	// loop re-running the same tool calls is caught as no-progress rather than left to
	// burn its whole step budget. It reads the record the run already writes, so it adds
	// no obligation on the executor.
	cfg.Progress = progress.NewSpineProbe(log, workdir)
	rt, err := runtime.New(cfg)
	if err != nil {
		return nil, err
	}
	return &missionRun{rt: rt, sess: parts.sess, parts: parts}, nil
}

// assembleToolsetMission wires one goal runtime over a caller-supplied toolset and
// the grant bound to it, recording onto the spine exactly as assembleMission does.
// There is no sandbox: the toolset holds every action the run may take, and the
// grant it arrived with is the complete authority the waist consults. Budget,
// brakes, governance recording, and compaction are identical to the sandboxed
// path, so a specialised run is not a less-governed run.
func assembleToolsetMission(model llm.Model, plan harness.Plan, ts *boundToolset, system string, rstore resource.Store, jq jobs.Queue, log spine.Log, runID string, planning bool) (*missionRun, error) {
	var sopts []session.Option
	if runID != "" {
		sopts = append(sopts, session.WithID(runID))
	}
	sess := session.New(log, bus.NewMemory(), sopts...)
	parts := &missionParts{
		sess:    sess,
		toolset: ts.tools,
		grant:   ts.grant,
		sink:    spinesink.New(log, sess.ID()),
	}

	opts := []mission.Option{
		mission.WithTools(parts.toolset...),
		mission.WithSystem(system),
		mission.WithObserver(parts.sess.Reporter()),
		mission.WithGrant(parts.grant),
		mission.WithBudget(budgetpkg.NewHook(rstore)),
		mission.WithBrakes(defaultBrakes()),
		mission.WithEventSink(parts.sink),
		mission.WithCompactionBudget(defaultCompactionBudget),
	}
	opts = append(opts, mission.PlanOptions(plan)...)
	exec := mission.NewExecutor(model, opts...)
	cfg := parts.runtimeConfig(exec, mission.Convergence{}, rstore, jq)
	if planning {
		cfg.Planner = mission.NewPlanner(model)
	}
	rt, err := runtime.New(cfg)
	if err != nil {
		return nil, err
	}
	return &missionRun{rt: rt, sess: parts.sess, parts: parts}, nil
}

// externalModel returns the model string an external agent CLI should drive, or empty
// when the run is native. It is set on the submitted goal so the episode driver hands
// it to the CLI.
func externalModel(ea *externAgent) string {
	if ea == nil {
		return ""
	}
	return ea.model
}

// assembleExternalMission wires a one-shot run whose loop is an external agent CLI: it
// shares the same sandbox, session, toolset, capability grant, and governance
// recording as assembleMission (via newMissionParts), but builds the run loop from the
// external agent's driver instead of a native model executor. The driver's episode
// loop routes every tool call the CLI makes back through the same dispatch waist, so
// the grant, containment gate, safety brake, and spend ceiling bound the external
// harness exactly as they bound a native loop; the CLI's own inner model calls stay
// unobserved-but-contained and are recorded as a declared provenance gap. A run driven
// this way does not fan out (the external harness owns its own loop), so the spawn
// action is withheld from the grant.
func assembleExternalMission(ea *externAgent, workdir, system string, rstore resource.Store, jq jobs.Queue, log spine.Log, skills state.SkillStore, runID string, resLimits sandbox.ResourceLimits) (*missionRun, error) {
	parts, err := newMissionParts(workdir, log, skills, runID, false, resLimits)
	if err != nil {
		return nil, err
	}

	// Record the harness's own account of its episode onto the same stream the waist
	// records the run's enforced effects on. The stream exists only now (the driver was
	// built during detection, before a run was assembled), which is why the sink is bound
	// here rather than at construction.
	recordAttestedEvents(ea, log, parts.sess.ID())

	// The loop-agnostic Spec the driver builds from: the same governance ingredients a
	// native run assembles, carried to the external episode loop. Model is intentionally
	// unset (an llm.Model), since the external CLI drives its own model, selected by the
	// goal's model string; the compaction budget and scaffolding plan are native-loop
	// concerns the external CLI manages itself, so they are left unset.
	exec, stop, err := ea.driver.Build(driver.Spec{
		Tools:     parts.toolset,
		System:    system,
		Grant:     parts.grant,
		HasGrant:  true,
		Sandbox:   parts.sandbox,
		Reporter:  parts.sess.Reporter(),
		EventSink: parts.sink,
		Brakes:    defaultBrakes(),
		Budget:    budgetpkg.NewHook(rstore),
	})
	if err != nil {
		return nil, err
	}
	rt, err := runtime.New(parts.runtimeConfig(exec, stop, rstore, jq))
	if err != nil {
		return nil, err
	}
	return &missionRun{rt: rt, sess: parts.sess, parts: parts}, nil
}

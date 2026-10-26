package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	budgetpkg "github.com/ionalpha/flynn/budget"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/harness"
	"github.com/ionalpha/flynn/internal/spinesink"
	"github.com/ionalpha/flynn/jobs"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/netguard"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/spine"
)

// defaultSystemPrompt frames the agent for a coding/automation task. It is kept
// short on purpose: a capable model works better from a clear goal than from a long
// list of rules.
const defaultSystemPrompt = `You are Flynn, an autonomous agent. You take on whatever objective the user gives you, from writing, research, and analysis to planning and software work.
You have tools to run shell commands and to read, write, edit, glob, and grep files in a sandboxed working directory; every command and file path is confined to it. Use the tools when the task calls for them, and answer directly when it does not.
Work toward the objective directly: gather what you need, do the work, and verify it with the tools rather than guessing.
When the objective is fully accomplished, stop and reply with a short summary of what you did.`

// defaultCompactionBudget is the input-token budget at which the CLI starts eliding
// the oldest middle turns from a long session. It is a conservative floor for the
// large hosted models the CLI targets by default (roughly half a 200k window);
// per-model, window-aware triggering arrives with the model registry.
const defaultCompactionBudget = 100_000

// drive assembles the runtime over the given store and the sandboxed toolset,
// streams the session live to out, and returns the converged result, the session
// id (used as learning provenance), and the conversation transcript (so the
// distiller can learn from how the goal was reached, not just the final summary).
// The system prompt is supplied so the caller can fold recalled knowledge into it.
//
// It is the positional spelling of [driveRequest.execute], kept while its callers move
// to building the request directly.
func drive(ctx context.Context, out io.Writer, model llm.Model, plan harness.Plan, workdir, objective, system string, rstore resource.Store, jq jobs.Queue, log spine.Log, verbose bool, resumeID string, fanout *fanoutConfig, opts ...driveOption) (result, source string, transcript []llm.Message, err error) {
	req := driveRequest{
		out: out, model: model, plan: plan, workdir: workdir, objective: objective, system: system,
		rstore: rstore, jq: jq, log: log, verbose: verbose, resumeID: resumeID, fanout: fanout,
	}
	for _, o := range opts {
		o(&req.cfg)
	}
	return req.execute(ctx)
}

// driveRequest is everything one driven run needs: what to run it on, what to ask it,
// and the optional levers in cfg.
type driveRequest struct {
	out       io.Writer
	model     llm.Model
	plan      harness.Plan
	workdir   string
	objective string
	system    string
	rstore    resource.Store
	jq        jobs.Queue
	log       spine.Log
	verbose   bool
	// resumeID continues an existing run instead of opening a new one.
	resumeID string
	// fanout, when set, drives the full goals engine (the Router plus a delegation
	// spawner) rather than a single governed conversation. Both seal into the same
	// verifiable record, so fan-out adds delegation without changing how a run is
	// recorded or checked.
	fanout *fanoutConfig
	cfg    driveConfig
}

// execute runs the request to completion and returns the result, the run's session id and
// the transcript. See [drive].
func (r driveRequest) execute(ctx context.Context) (result, source string, transcript []llm.Message, err error) {
	w := &syncWriter{w: r.out}
	run, err := r.assemble()
	if err != nil {
		return "", "", nil, err
	}
	defer func() { _ = run.Close() }()
	r.announce(w, run)

	if err := r.openBudget(ctx, run); err != nil {
		return "", "", nil, err
	}

	// Seed the driving context with an egress observer bound to the run's stream, so every
	// dial netguard makes on the run's behalf (a hosted model's API call, say) reports its
	// allow/block verdict into the same recorded history as the run's governed actions. A
	// run whose model is local makes no netguard-gated dial, so nothing is recorded.
	egress := spinesink.NewEgress(r.log, run.sess.ID())
	runCtx, cancel := context.WithCancel(netguard.WithObserver(ctx, egress.Observe))
	defer cancel()
	done := make(chan struct{})
	go func() { _ = run.rt.Start(runCtx); close(done) }()

	events, err := run.sess.Subscribe(runCtx, 0)
	if err != nil {
		return "", "", nil, err
	}
	if err := r.begin(runCtx, run); err != nil {
		return "", "", nil, err
	}

	result, transcript, _, runErr := renderStream(w, events, r.verbose, r.cfg.observe)
	if r.cfg.extAgent != nil {
		r.declareProvenance(runCtx, w, run)
	}

	cancel()
	<-done
	return result, run.sess.ID(), transcript, runErr
}

// assemble builds the mission the request describes. A caller-supplied toolset, an
// external agent CLI, fan-out and the plain conversation each get their own assembly, and
// all of them record the same session and apply the same governance.
func (r driveRequest) assemble() (*missionRun, error) {
	c := r.cfg
	switch {
	case c.toolset != nil:
		// No sandbox and no working-tree tools: the caller supplies the toolset and grant.
		return assembleToolsetMission(r.model, r.plan, c.toolset, r.system, r.rstore, r.jq, r.log, r.resumeID, c.planning, c.gates)
	case c.extAgent != nil:
		// The CLI's episode driver runs the loop in place of a model conversation.
		return assembleExternalMission(c.extAgent, r.workdir, r.system, r.rstore, r.jq, r.log, c.skills, r.resumeID, c.shell)
	case r.fanout != nil:
		return assembleFanoutMission(r.model, r.plan, r.workdir, r.system, r.rstore, r.jq, r.log, c.skills, r.resumeID, r.fanout.resolveModel, c.shell, c.gates)
	default:
		return assembleMission(r.model, r.plan, r.workdir, r.system, r.rstore, r.jq, r.log, c.skills, r.resumeID, c.shell, c.planning, c.proof, c.gates)
	}
}

// announce tells the operator, before the run starts, what they cannot see from the
// command line: that it will stop for a person and on what (a run that halts halfway
// with no warning reads as a hang), how its ledger behaves, and whether it picked up the
// terms from their file and which of them carry a check it can run rather than a
// judgement the auditor model will make.
func (r driveRequest) announce(w io.Writer, run *missionRun) {
	c := r.cfg
	_, _ = fmt.Fprintf(w, "  run %s\n", run.sess.ID())
	if gated := gatedActions(approvalPolicy(c.gates.approve)); len(gated) > 0 {
		how := "refused, nothing here can prompt"
		if c.gates.prompter != nil {
			how = "paused for your decision"
		}
		_, _ = fmt.Fprintf(w, "  approval required (%s): %s\n", how, strings.Join(gated, ", "))
	}
	if line := ledgerLine(c.planning || r.fanout != nil, c.proof || r.fanout != nil); line != "" {
		_, _ = fmt.Fprintln(w, line)
	}
	for _, line := range termsLines(c.terms) {
		_, _ = fmt.Fprintln(w, line)
	}
}

// openBudget opens the run's spend pool before the goal is submitted, so the ceiling is
// in force from the first action rather than after a race. The pool is keyed by the run
// id (the root goal's name, which equals the session id), and every fan-out child
// inherits it, so one budget bounds the whole run. Without a ceiling nothing is opened
// and the always-wired budget hook is inert (an absent pool is unlimited).
func (r driveRequest) openBudget(ctx context.Context, run *missionRun) error {
	if !r.cfg.budgeted() {
		return nil
	}
	if _, err := budgetpkg.NewLedger(r.rstore).Open(ctx, run.sess.ID(), resource.Scope{}, r.cfg.budget); err != nil {
		return fmt.Errorf("open run budget: %w", err)
	}
	return nil
}

// begin starts the run's goal. A resumed run re-drives its existing goal, preserving its
// recorded progress, rather than opening a new one; the subscription taken before this
// replays the prior conversation first, then tails the rest live.
func (r driveRequest) begin(ctx context.Context, run *missionRun) error {
	if r.resumeID != "" {
		g, err := run.rt.Resume(ctx, r.resumeID)
		if err != nil {
			return err
		}
		run.sess.Resume(ctx, run.rt, g.Key())
		return nil
	}
	_, err := run.sess.Submit(ctx, run.rt, r.goalSpec())
	return err
}

// goalSpec is the root goal of a new run.
func (r driveRequest) goalSpec() goal.Spec {
	return goal.Spec{
		Objective: r.objective,
		// A goal spec file may say what being finished means in the operator's own words,
		// which is what the stop evaluator reads.
		StopCondition: stopCondition(r.cfg.stopCondition),
		// Empty for a native run (the host default model applies); for an external agent it
		// is the CLI's model string, which the episode driver hands the CLI so
		// `flynn --model codex:<model>` pins that model.
		Model: externalModel(r.cfg.extAgent),
		// A fan-out parent spends steps a single conversation does not: one dispatching each
		// delegation and one per poll while it waits for the children (a wait makes no model
		// call, but the reconciler still counts it). The larger budget keeps a legitimate
		// delegation from being cut off mid-fold; the safety brake and fan-out width still
		// bound a runaway.
		MaxSteps: fanoutMaxSteps(r.fanout),
		// Declared on the goal rather than the executor because the pause that answers a
		// missing allowance is the reconciler's, and it reads the declarations off the goal
		// to tell an ask that has been answered from one that has not.
		Allowances: r.cfg.allowances,
		// Audited on every reconcile before the stop condition is consulted, so a breach
		// settles the goal rather than being weighed against finishing.
		Invariants: r.cfg.terms,
	}
}

// declareProvenance records on the run's stream, before it is sealed, that an external
// agent harness drove the loop. The record then vouches for enforced effects (every tool
// call crossed the dispatch waist) while naming the harness's inner reasoning as an
// unobserved gap, so an external run never claims the integrity of a native one. A failed
// run declares it too: the declaration's absence is what marks a record as natively
// driven, so omitting it would seal a broken external episode as though Flynn's own loop
// had run it. Best effort: a failure to record is reported, not fatal.
func (r driveRequest) declareProvenance(ctx context.Context, w io.Writer, run *missionRun) {
	if err := appendProvenance(ctx, r.log, run.sess.ID(), observedProvenance(r.cfg.extAgent)); err != nil {
		_, _ = fmt.Fprintf(w, "  (provenance not recorded: %v)\n", err)
	}
	// A harness event the record could not hold is a hole in the harness's account. The
	// declared count still names every event the harness reported, so verify reports the
	// gap from the record alone; saying it here tells the operator why.
	if lost, err := unrecordedAttested(r.cfg.extAgent); lost > 0 {
		_, _ = fmt.Fprintf(w, "  (%d attested event(s) not recorded: %v)\n", lost, err)
	}
}

// defaultStopCondition is what a run means by finished when nobody says otherwise: the
// objective, judged achieved. It is deliberately the run's own account of its work, which
// is why a goal's terms are audited separately and first.
const defaultStopCondition = "the objective is fully accomplished"

// stopCondition returns the operator's stop condition, or the default where they stated
// none.
func stopCondition(stated string) string {
	if s := strings.TrimSpace(stated); s != "" {
		return s
	}
	return defaultStopCondition
}

// ledgerLine is what a run says about its ledger before it starts: whether each
// planned item's declared check runs, and whether an item the record cannot show a
// passing check for will stop the run.
//
// It exists because a capability that is available and off is invisible otherwise.
// Requiring proof is the one thing in the boundary register with a `staged` verdict:
// the producer ships, the loop runs and records verdicts on every planned goal, and
// the refusal that reads those verdicts is behind `--require-proof` until enough real
// runs show items flipping to proven. An operator who cannot see that from the run
// has no way to know the dial exists, and a capability nobody can see is how
// default-off becomes permanent without anyone deciding it should.
//
// A goal that was never planned has no ledger and says nothing here.
func ledgerLine(planned, proof bool) string {
	switch {
	case !planned:
		return ""
	case proof:
		return "  ledger: each item's check runs, and an item with no passing check on the record stops the run"
	}
	return "  ledger: each item's check runs and is recorded; --require-proof also stops the run on an item it cannot prove"
}

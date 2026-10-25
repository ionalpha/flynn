package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/governed"
	"github.com/ionalpha/flynn/harness"
	"github.com/ionalpha/flynn/jobs"
	"github.com/ionalpha/flynn/llm"
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

// withOptions applies opts to the request's levers and returns the request, for a caller
// that gathers its options as a slice.
func (r driveRequest) withOptions(opts ...driveOption) driveRequest {
	for _, o := range opts {
		o(&r.cfg)
	}
	return r
}

// execute assembles the runtime over the request's store and the sandboxed toolset,
// streams the session live to out, and returns the converged result, the session id (used
// as learning provenance), and the conversation transcript (so the distiller can learn from
// how the goal was reached, not just the final summary). The system prompt is supplied so
// the caller can fold recalled knowledge into it.
func (r driveRequest) execute(ctx context.Context) (result, source string, transcript []llm.Message, err error) {
	w := &syncWriter{w: r.out}
	run, err := r.assembly().assemble(ctx)
	if err != nil {
		return "", "", nil, err
	}
	defer func() { _ = run.Close() }()
	r.announce(w, run)

	// Open the spend ceiling and seed the driving context with an egress observer bound to
	// the run's stream, so every dial netguard makes on the run's behalf (a hosted model's
	// API call, say) reports its allow/block verdict into the same recorded history as the
	// run's governed actions. A run whose model is local makes no netguard-gated dial, so
	// nothing is recorded.
	prepared, err := run.Prepare(ctx)
	if err != nil {
		return "", "", nil, err
	}
	runCtx, cancel := context.WithCancel(prepared)
	defer cancel()
	done := make(chan struct{})
	go func() { _ = run.Runtime().Start(runCtx); close(done) }()

	events, err := run.Session().Subscribe(runCtx, 0)
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
	return result, run.ID(), transcript, runErr
}

// assembly is the request as the governed-run assembly takes it. A caller-supplied
// toolset, an external agent CLI, fan-out and the plain conversation each get their own
// mode of it, and all of them record the same session and apply the same governance.
func (r driveRequest) assembly() assembly {
	c := r.cfg
	return assembly{
		model: r.model, plan: r.plan, workdir: r.workdir, system: r.system,
		rstore: r.rstore, jq: r.jq, log: r.log, skills: c.skills, runID: r.resumeID,
		resLimits: c.resLimits, planning: c.planning, proof: c.proof, gates: c.gates,
		budget: c.budget, fanout: r.fanout, ext: c.extAgent, toolset: c.toolset,
	}
}

// announce tells the operator, before the run starts, what they cannot see from the
// command line: that it will stop for a person and on what (a run that halts halfway
// with no warning reads as a hang), how its ledger behaves, and whether it picked up the
// terms from their file and which of them carry a check it can run rather than a
// judgement the auditor model will make.
func (r driveRequest) announce(w io.Writer, run *governed.Run) {
	c := r.cfg
	_, _ = fmt.Fprintf(w, "  run %s\n", run.ID())
	if gated := governed.GatedActions(governed.ApprovalPolicy(c.gates.approve)); len(gated) > 0 {
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

// begin starts the run's goal. A resumed run re-drives its existing goal, preserving its
// recorded progress, rather than opening a new one; the subscription taken before this
// replays the prior conversation first, then tails the rest live.
func (r driveRequest) begin(ctx context.Context, run *governed.Run) error {
	if r.resumeID != "" {
		return run.Resume(ctx, r.resumeID)
	}
	_, err := run.Submit(ctx, r.goalSpec())
	return err
}

// goalSpec is the root goal of a new run, before the run fills the defaults its mode
// carries: the external agent's model string, and the larger step budget a fan-out parent
// spends dispatching each delegation and polling for its children.
func (r driveRequest) goalSpec() goal.Spec {
	return goal.Spec{
		Objective: r.objective,
		// A goal spec file may say what being finished means in the operator's own words,
		// which is what the stop evaluator reads.
		StopCondition: strings.TrimSpace(r.cfg.stopCondition),
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
func (r driveRequest) declareProvenance(ctx context.Context, w io.Writer, run *governed.Run) {
	if err := appendProvenance(ctx, r.log, run.ID(), observedProvenance(r.cfg.extAgent)); err != nil {
		_, _ = fmt.Fprintf(w, "  (provenance not recorded: %v)\n", err)
	}
	// A harness event the record could not hold is a hole in the harness's account. The
	// declared count still names every event the harness reported, so verify reports the
	// gap from the record alone; saying it here tells the operator why.
	if lost, err := unrecordedAttested(r.cfg.extAgent); lost > 0 {
		_, _ = fmt.Fprintf(w, "  (%d attested event(s) not recorded: %v)\n", lost, err)
	}
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

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
func drive(ctx context.Context, out io.Writer, model llm.Model, plan harness.Plan, workdir, objective, system string, rstore resource.Store, jq jobs.Queue, log spine.Log, verbose bool, resumeID string, fanout *fanoutConfig, opts ...driveOption) (result, source string, transcript []llm.Message, err error) {
	w := &syncWriter{w: out}
	var cfg driveConfig
	for _, o := range opts {
		o(&cfg)
	}
	// A run with fan-out enabled drives the full goals engine (the Router plus a
	// delegation spawner); otherwise it is a single governed conversation. Both seal
	// into the same verifiable record, so fan-out adds delegation without changing how
	// a run is recorded or checked.
	var run *missionRun
	switch {
	case cfg.toolset != nil:
		// A caller-supplied toolset and grant: no sandbox, no working-tree tools, the
		// same session recording and governance as every other path.
		run, err = assembleToolsetMission(model, plan, cfg.toolset, system, rstore, jq, log, resumeID, cfg.planning, cfg.gates)
	case cfg.extAgent != nil:
		// An external agent CLI drives the loop: the same sandbox, session, toolset,
		// grant, and governance recording as a native run, but the run loop is the CLI's
		// episode driver rather than a model conversation.
		run, err = assembleExternalMission(cfg.extAgent, workdir, system, rstore, jq, log, cfg.skills, resumeID, cfg.resLimits)
	case fanout != nil:
		run, err = assembleFanoutMission(model, plan, workdir, system, rstore, jq, log, cfg.skills, resumeID, fanout.resolveModel, cfg.resLimits, cfg.gates)
	default:
		run, err = assembleMission(model, plan, workdir, system, rstore, jq, log, cfg.skills, resumeID, cfg.resLimits, cfg.planning, cfg.proof, cfg.gates)
	}
	if err != nil {
		return "", "", nil, err
	}
	defer func() { _ = run.Close() }()
	_, _ = fmt.Fprintf(w, "  run %s\n", run.sess.ID())
	// Say up front that the run will stop for a person, and on what. A run that halts
	// halfway through with no warning reads as a hang, and one that refuses an action
	// because nobody could be asked should have said so before it started.
	if gated := gatedActions(approvalPolicy(cfg.gates.approve)); len(gated) > 0 {
		how := "refused, nothing here can prompt"
		if cfg.gates.prompter != nil {
			how = "paused for your decision"
		}
		_, _ = fmt.Fprintf(w, "  approval required (%s): %s\n", how, strings.Join(gated, ", "))
	}
	if line := ledgerLine(cfg.planning || fanout != nil, cfg.proof || fanout != nil); line != "" {
		_, _ = fmt.Fprintln(w, line)
	}
	// Read the run's terms back before it starts. The operator wrote them in a file, and
	// the two things they cannot see from the file are whether this run actually picked
	// them up and which of them carries a check it can run rather than a judgement the
	// auditor model will make.
	for _, line := range termsLines(cfg.terms) {
		_, _ = fmt.Fprintln(w, line)
	}

	// Open the run's spend pool before the goal is submitted, so the ceiling is in
	// force from the first action rather than after a race. The pool is keyed by the
	// run id (the root goal's name, which equals the session id), and every fan-out
	// child inherits it, so one budget bounds the whole run. Without a ceiling nothing
	// is opened and the always-wired budget hook is inert (an absent pool is unlimited).
	if cfg.budgeted() {
		if _, oerr := budgetpkg.NewLedger(rstore).Open(ctx, run.sess.ID(), resource.Scope{}, cfg.budget); oerr != nil {
			return "", "", nil, fmt.Errorf("open run budget: %w", oerr)
		}
	}

	// Record the run's own outbound-network decisions onto its stream: seed the driving
	// context with an egress observer bound to the run's stream, so every dial netguard
	// makes on the run's behalf (a hosted model's API call, say) reports its allow/block
	// verdict into the same recorded history as the run's governed actions. A run whose
	// model is local makes no netguard-gated dial, so nothing is recorded.
	egress := spinesink.NewEgress(log, run.sess.ID())
	runCtx, cancel := context.WithCancel(netguard.WithObserver(ctx, egress.Observe))
	defer cancel()
	done := make(chan struct{})
	go func() { _ = run.rt.Start(runCtx); close(done) }()

	events, err := run.sess.Subscribe(runCtx, 0)
	if err != nil {
		return "", "", nil, err
	}
	if resumeID != "" {
		// Continue an existing run: re-drive its goal (preserving its recorded
		// progress) rather than opening a new one. Subscribe above replays the prior
		// conversation first, then tails the rest live.
		g, err := run.rt.Resume(runCtx, resumeID)
		if err != nil {
			return "", "", nil, err
		}
		run.sess.Resume(runCtx, run.rt, g.Key())
	} else if _, err := run.sess.Submit(runCtx, run.rt, goal.Spec{
		Objective: objective,
		// What being finished means. A goal spec file may say it in the operator's own
		// words, which is the half of the run's terms the stop evaluator reads; with no
		// file, every run has said this.
		StopCondition: stopCondition(cfg.stopCondition),
		// The model the loop drives. Empty for a native run (the host default model
		// applies); for an external agent it is the CLI's model string, which the episode
		// driver hands the CLI so `flynn --model codex:<model>` pins that model.
		Model: externalModel(cfg.extAgent),
		// A fan-out parent spends steps it would not as a single conversation: a step
		// dispatching each delegation, and a step per poll while it waits for the
		// children to finish (a wait makes no model call, but the reconciler still
		// counts it). Give it a larger budget so a legitimate delegation that waits on
		// a few children is not cut off mid-fold; a single conversation keeps the
		// default. The safety brake and fan-out width still bound a runaway.
		MaxSteps: fanoutMaxSteps(fanout),
		// The irreversible actions outside the workspace this run was declared to be
		// allowed. They ride on the goal rather than on the executor because the pause
		// that answers a missing one is the reconciler's, and it reads the declarations
		// off the goal to tell an ask that has been answered from one that has not.
		Allowances: cfg.allowances,
		// The terms of the run: what must stay true while it works. They are audited on
		// every reconcile before the stop condition is consulted, so a breach settles the
		// goal rather than being weighed against finishing.
		Invariants: cfg.terms,
	}); err != nil {
		return "", "", nil, err
	}

	result, transcript, _, runErr := renderStream(w, events, verbose, cfg.observe)

	// Declare the run's provenance onto its stream before it is sealed, when an external
	// agent harness drove the loop. The record then vouches for enforced effects (every
	// tool call crossed the dispatch waist) while naming the harness's inner reasoning as
	// an unobserved gap, so an external run never claims the integrity of a native one.
	// It is appended before cancel() so the run context is still live; a native run
	// records nothing. Best effort: a failure to record it is reported, not fatal.
	//
	// A failed run declares its provenance too. The declaration's absence is what marks a
	// record as natively driven, so omitting it on failure would seal a broken external
	// episode as though Flynn's own loop had run it: the exact overclaim this declaration
	// exists to prevent.
	if cfg.extAgent != nil {
		if perr := appendProvenance(runCtx, log, run.sess.ID(), observedProvenance(cfg.extAgent)); perr != nil {
			_, _ = fmt.Fprintf(w, "  (provenance not recorded: %v)\n", perr)
		}
		// A harness event the record could not hold is a hole in the harness's account. The
		// declared count still names every event the harness reported, so verify reports the
		// gap from the record alone; saying it here tells the operator why.
		if lost, lerr := unrecordedAttested(cfg.extAgent); lost > 0 {
			_, _ = fmt.Fprintf(w, "  (%d attested event(s) not recorded: %v)\n", lost, lerr)
		}
	}

	cancel()
	<-done
	return result, run.sess.ID(), transcript, runErr
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

package main

import (
	"context"
	"time"

	"github.com/ionalpha/flynn/brakes"
	budgetpkg "github.com/ionalpha/flynn/budget"
	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/driver"
	"github.com/ionalpha/flynn/evidence"
	"github.com/ionalpha/flynn/harness"
	"github.com/ionalpha/flynn/jobs"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/orchestration"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/runtime"
	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/skill/skilltool"
	"github.com/ionalpha/flynn/spine"
)

// defaultFanoutWidth caps how many child runs a fan-out may have outstanding at
// once, bounding the blast radius of delegation alongside the depth guard.
const defaultFanoutWidth = 8

// fanoutRootMaxSteps is the step budget a fan-out parent runs under. A fan-out adds
// orchestration steps a single conversation never takes (a dispatch per delegation,
// and one poll per reconcile while it waits for the children), so the default budget
// that suits a single loop would cut a legitimate delegation off mid-fold. Zero
// (single conversation) keeps the reconciler's default.
const fanoutRootMaxSteps = 200

// fanoutMaxSteps returns the step budget for a run's root goal: the larger fan-out
// budget when delegation is enabled, or zero (the reconciler's default) otherwise.
func fanoutMaxSteps(fanout *fanoutConfig) int {
	if fanout != nil {
		return fanoutRootMaxSteps
	}
	return 0
}

// defaultMaxActionsPerMinute is the rate the default safety brake halts a run at.
// It is set well above any real run's pace so it catches only a degenerate tight
// loop, not legitimate tool use.
const defaultMaxActionsPerMinute = 600

// defaultBrakes builds the run's safety governor: the circuit breaker that halts a
// runaway from outside the model loop. It is a generous rate backstop (a real run
// dispatches far fewer than this per minute), so it fires only on a degenerate tight
// loop, never on legitimate tool use. Every run gets one, the single conversation as
// much as the fan-out, so no run can spin unbounded even when the model is jailbroken
// or looping. Each call returns a fresh Hook (with its own in-memory kill-switch), so
// a run's breaker state and halt are its own.
func defaultBrakes() *brakes.Hook {
	return brakes.NewHook(brakes.Limits{MaxActions: defaultMaxActionsPerMinute, Window: time.Minute}, nil)
}

// fanoutConfig enables the goals engine on a one-shot run: the model may delegate
// self-contained sub-goals to concurrent, governed child agents, and each child is
// routed to the model and loop its bound Agent archetype pins (resolveModel turns a
// named model into a client). Every child runs under a grant narrowed from the
// parent's, shares the run's budget, and folds back into the parent's single sealed
// record, so a multi-goal, multi-model fan-out stays one verifiable run. A nil
// *fanoutConfig leaves a run as a single conversation, which is the n=1 case of the
// same mechanism.
type fanoutConfig struct {
	resolveModel driver.ModelResolver
}

// assembleFanoutMission wires a one-shot run that drives the full goals engine over
// the durable store: a Router that builds one loop per (driver, model) a goal
// selects, and a fan-out spawner that creates governed child goals. It mirrors
// assembleMission (same sandbox, session, toolset, grant, governance recording, and
// compaction) but adds the spawn action to the grant and routes each goal through
// the Router, so a delegated child runs as the agent its archetype names, on that
// agent's model, while the root and every child fold into one recorded, sealable
// stream. The shared store backs the child goals a fan-out spawns, so they land
// where the runtime reconciles them.
func assembleFanoutMission(model llm.Model, plan harness.Plan, workdir, system string, rstore resource.Store, jq jobs.Queue, log spine.Log, skills *skilltool.Set, runID string, resolveModel driver.ModelResolver, resLimits sandbox.ResourceLimits, appr approvalSetup) (*missionRun, error) {
	parts, err := newMissionParts(workdir, log, skills, runID, true, resLimits)
	if err != nil {
		return nil, err
	}
	// Approval is carried in the Router's base spec, so it applies to every loop the
	// Router builds: the root's and each delegated child's. A gate that stopped at the
	// root would be a gate a run walks around by delegating.
	stack, err := newApprovalStack(appr.actions, log, parts.sess.ID())
	if err != nil {
		return nil, err
	}

	// The spawner is the run's fan-out: it creates governed child goals (owned by the
	// parent, grant narrowed, depth- and concurrency-bounded) and hands them to the
	// runtime. Its enqueue hook is bound once the runtime exists (below).
	spawner := orchestration.NewSpawner(rstore, nil, orchestration.WithConcurrency(defaultFanoutWidth))

	// The brake and the spend pool are built once and shared by every loop the Router
	// builds and by the plan-driven fan-out below, so a halt or a ceiling bounds the whole
	// run rather than one path through it.
	brk := defaultBrakes()
	pool := budgetpkg.NewHook(rstore)

	// The Router drives each goal through the loop and model its spec selects: the
	// default loop and host model for the root, and the bound Agent's loop and model
	// for a delegated child. The shared ingredients (tools, default prompt and grant,
	// sandbox gate, governance recording, compaction, brake, fan-out) apply to every
	// loop; the per-goal prompt, grant, and model are applied from the goal.
	router := driver.NewRouter(driver.RouterConfig{
		Registry:     driver.Default(),
		DefaultModel: model,
		ResolveModel: resolveModel,
		Base: driver.Spec{
			Tools:    parts.toolset,
			System:   system,
			Grant:    parts.grant,
			HasGrant: true,
			Sandbox:  parts.sandbox,
			Reporter: parts.sess.Reporter(),
			Fanout:   spawner,
			// Record every governed action's lifecycle onto the run's own stream, so the
			// admission decisions (including each delegation) are part of the sealed record.
			EventSink:        parts.sink,
			CompactionBudget: defaultCompactionBudget,
			// Halt a runaway from outside the model loop: the same circuit breaker the
			// single conversation runs under, shared by every child (which run under this
			// pool), so the whole fan-out is braked as one.
			Brakes: brk,
			// Charge every action (root and every child, which share one pool) against the
			// run's spend pool, so a ceiling set for the run halts the whole fan-out. Inert
			// until a budget is opened: an absent pool is unlimited.
			Budget: pool,
			// Pause a privileged action the run's policy lists until a person allows it,
			// on the root and on every delegated child alike.
			Approval: stack.spec(appr.prompter),
			// Apply the model's scaffolding plan so a weaker model is driven with the
			// support it needs; the zero plan of a strong model adds nothing.
			Plan: plan,
		},
	})

	cfg := parts.runtimeConfig(router, router, rstore, jq)
	// Run the units of a goal's plan as governed child goals, in dependency order,
	// through the same spawner the model's own delegation uses. Without this a goal that
	// carries a unit graph stalls saying no spawner is wired, which is an honest refusal
	// on a capability the binary otherwise has: the graph is admitted and validated at
	// submit time and then has nothing to run it.
	cfg.Units = orchestration.Units(spawner, orchestration.UnitGovernor(parts.sandbox, brk, pool))
	// A unit is settled from its child's ledger, so the fan-out needs the evidence loop
	// closed or every unit fails as unproven: the child carries the unit's verify clause
	// as a ledger item, and without a verifier nothing ever runs that check. The clause is
	// plan-authored, so it runs in the run's own sandbox under the same containment gate
	// as the agent's shell tool.
	//
	// The governance event sink is left off here for the reason assembleMission leaves it
	// off: a second dispatcher's correlation ids are monotonic only within itself, so two
	// dispatchers writing lifecycle events onto one stream emit colliding call ids. The
	// verdict is on the record as its own item-verified event, which is the part the
	// evidence gate reads.
	cfg.Verifier = evidence.NewCommandVerifier(parts.sandbox,
		dispatch.WithAdmitter(capability.Admitter{}),
		dispatch.WithHook(capability.NewContainmentGate(parts.sandbox)))
	cfg.Evidence = evidence.NewSpineEvidence(log)
	// Make the settled ledger, not the model's final answer, decide whether a goal
	// carrying one is done. A unit is settled from its child's ledger, so without this a
	// child converges the moment the model says it is finished and the unit fails as
	// unproven every time: the check the plan author wrote would never get a turn to run.
	// A goal with no ledger, which is every goal on this path that is not a unit's child,
	// is unaffected.
	cfg.RequireLedgerProof = true
	rt, err := runtime.New(cfg)
	if err != nil {
		return nil, err
	}
	// Bind the spawner to the runtime so a spawned child is enqueued for
	// reconciliation. Binding here (rather than at construction) breaks the cycle: the
	// executor holds the spawner, and the runtime holds the executor.
	spawner.SetEnqueue(func(ctx context.Context, key resource.Key) error {
		_, rerr := rt.Resume(ctx, key.Name)
		return rerr
	})
	return &missionRun{rt: rt, sess: parts.sess, parts: parts}, nil
}

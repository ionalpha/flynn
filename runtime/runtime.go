// Package runtime assembles the agent's goal control plane into one startable
// unit: the resource store, the durable job queue, the message bus, the reconcile
// manager, and the goal reconciler and step worker, wired together.
//
// It owns composition, not behaviour. What a goal step does and how convergence is
// judged are injected (a goal.StepExecutor and a goal.StopEvaluator), so the same
// plumbing runs with a stub in tests and with a model-backed executor in
// production. The result is something you can start, submit a goal to, and watch
// drive itself to convergence: the reconciler dispatches a step, the worker runs
// it and signals completion, the signal re-triggers the reconciler, and resync
// guarantees progress even if a signal is lost. Because each step of progress is
// recorded on the durable store and queue, a restart resumes mid-goal rather than
// starting over.
package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ionalpha/flynn/bus"
	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/jobs"
	"github.com/ionalpha/flynn/reconcile"
	"github.com/ionalpha/flynn/resource"
)

// DefaultWorkerPoll is how often the step worker polls for work when idle. Both
// bundled queues implement jobs.Waker, so a fresh dispatch wakes the worker at
// once and this interval is only the fallback sweep for scheduled retries,
// expired leases, and enqueues from other processes.
const DefaultWorkerPoll = 50 * time.Millisecond

// Config assembles a Runtime. Executor and Stop are required: they are the agent's
// behaviour, which the foundation does not supply. Everything else has a
// standalone, in-process default.
type Config struct {
	// Executor performs one goal step. Required.
	Executor goal.StepExecutor
	// Stop decides whether a goal has converged. Required.
	Stop goal.StopEvaluator
	// Planner expands a goal's objective into its ledger before any building starts.
	// Setting it is what turns planning on: the runtime pairs it with the reconciler's
	// planning gate in one place (WithPlanner on the worker, WithPlanning on the
	// reconciler), so a goal can never be gated on a planning phase without a planner to
	// run it — the misconfiguration that would leave every goal unplanned and stalled.
	// Nil leaves planning off, so a goal runs exactly as before: no ledger, straight to
	// building.
	Planner goal.Planner
	// Progress detects a goal that has stopped getting anywhere: after each build step the
	// reconciler asks it for a fingerprint of the substantive work recorded so far, and
	// stops the goal once that fingerprint has not changed for a few consecutive steps,
	// with a no-progress reason rather than the budget reason a spinning loop would
	// eventually reach. Nil leaves detection off, so a goal is bounded only by its step
	// budget, exactly as before.
	Progress goal.ProgressProbe
	// Units runs the units of a goal's declared plan as governed child goals, in
	// dependency order. Setting it turns plan-driven fan-out on: a goal whose spec
	// carries a unit graph admits ready units through this rather than building, and it
	// converges only once every unit is proven.
	//
	// It is the plan-driven half of fan-out and it does not replace the model-driven
	// half: the spawn tool on the executor is untouched, and a goal carrying no units
	// runs exactly as before. Leaving it nil is not a way to ignore a plan, because a
	// goal that carries a graph with nothing wired to run it stalls saying so; a graph
	// admitted, validated and then quietly skipped is the failure that reads as a goal
	// which simply never fanned out.
	Units goal.UnitSpawner
	// Auditor rules on the terms a goal states beside its stop condition: after each
	// completed step the reconciler asks it whether any invariant has been broken, and a
	// breach stops the goal before the stop evaluator is consulted.
	//
	// Leaving it nil is not a way to run a goal's terms unchecked, because a goal that
	// states terms with no auditor wired stalls saying so. That is the same rule Units
	// follows, and the case for it is stronger: a fan-out that never happens is visible,
	// while a run that was never audited finishes looking exactly like one whose terms
	// held. A goal that states no terms is unaffected either way.
	Auditor goal.InvariantAuditor
	// Refusals reads the gates that refused this run, so a run that kept pushing on one
	// is stopped naming what refused it rather than being judged on whether it finished.
	//
	// Nil leaves detection off. Refusals are still recorded on the spine either way, so
	// this is a question of whether anything reads them as a verdict, not of whether the
	// run's governance decisions are kept.
	Refusals goal.RefusalProbe
	// Verifier runs a ledger item's declared check, and Evidence is the durable record
	// its verdict is written to and read back from. Setting both closes the ledger loop:
	// the run alternates building an item with running its check, and the verdicts on the
	// record settle the ledger through a default-FAIL evidence gate, so an item flips to
	// proven only from evidence the run actually produced. Whether an unsettled ledger
	// then refuses a completion claim is RequireLedgerProof.
	//
	// They are paired here for the reason the planner is: the producer lives on the
	// worker and the gate on the reconciler, and a goal gated on its ledger by a
	// reconciler whose worker cannot produce evidence would refuse to converge forever.
	// Wiring one side without the other is not a degraded mode, it is a hang, so it is
	// made unrepresentable in the one place that knows about both. Leaving either nil
	// leaves the loop open and a goal behaves exactly as it did before.
	Verifier goal.ItemVerifier
	Evidence goal.Evidence
	// RequireLedgerProof makes the settled ledger, not the model's final answer, decide
	// whether a planned goal is done: a completion claim over unproven items settles the
	// goal as stalled naming each one and why.
	//
	// It is staged behind the loop itself on purpose. Wiring Verifier and Evidence makes
	// items visibly flip to proven on real runs; this is then turned on against that
	// evidence rather than ahead of it, because a refusal switched on before anything
	// produces verifications stalls every goal. Leaving it off indefinitely is not an
	// option a composition has: a gate that is loaded and does nothing is the failure the
	// gate's self-test exists to catch.
	RequireLedgerProof bool
	// AllowAssertedEvidence lets an item be proven on a verification that was asserted
	// rather than run. It is off by default: a closed loop ships with a producer that
	// runs checks, which is exactly what makes requiring execution satisfiable, and an
	// unrunnable verify clause should fail the item rather than quietly pass it. Set it
	// only for a run whose checks genuinely cannot be executed, knowing that what the
	// ledger then records is a claim rather than a result.
	AllowAssertedEvidence bool

	// Store, Jobs, and Bus are the foundation ports. When Store is nil, an in-memory
	// store, queue, and bus are built over Clock with a registry holding the core
	// and Goal kinds. When Store is set, Jobs must be set too, and the registry the
	// store admits against must already include the Goal kind (goal.RegisterKind).
	Store resource.Store
	Jobs  jobs.Queue
	Bus   bus.Bus

	// Clock is the shared time source (default clock.System). A clock.Manual makes
	// resync and retry backoff deterministic in tests.
	Clock clock.Timing

	// Resync overrides the manager's safety-net interval. Non-positive values use
	// the default: the safety net cannot be turned off, because it is what makes
	// convergence a guarantee rather than a hope that no change hint is ever lost.
	Resync time.Duration
	// DriveSubmittedOnly narrows what the runtime drives to the goals it was given
	// itself (via SubmitGoal or Resume, which is also how a fan-out's children
	// arrive), so it never adopts a goal it merely finds in the store. A one-shot
	// command sets this: starting a run must not silently resume a goal an earlier
	// run left non-terminal, so each run keeps its own event stream and resuming a
	// parked run stays an explicit act. It narrows the resync sweep, it does not
	// disable it, so a lost change hint for the run's own goal is still recovered. A
	// long-lived server leaves it false and drives every goal in the store, which is
	// what resumes orphaned work after a crash.
	DriveSubmittedOnly bool
	// WorkerPoll overrides how often the step worker polls when idle.
	WorkerPoll time.Duration
	// PollInterval overrides how often the reconciler re-checks an in-flight step
	// absent a completion signal.
	PollInterval time.Duration
	// WorkerLease overrides how long a claimed step is leased. It bounds how long a
	// crashed worker's in-flight step waits before another worker re-leases it (0
	// uses the worker default).
	WorkerLease time.Duration
	// WorkerRetryBase and WorkerRetryCeiling bound the worker's exponential backoff
	// between failed step attempts (0 uses the worker defaults).
	WorkerRetryBase    time.Duration
	WorkerRetryCeiling time.Duration
	// StepMaxAttempts caps how many times a dispatched step is retried before it
	// goes dead and stalls the goal (0 uses the queue default).
	StepMaxAttempts int
}

// Runtime is the assembled goal control plane.
type Runtime struct {
	store      resource.Store
	jobs       jobs.Queue
	bus        bus.Bus
	manager    *reconcile.Manager
	worker     *goal.Worker
	clk        clock.Timing
	workerPoll time.Duration
	driven     *drivenSet
}

// drivenSet is the set of goals this process is responsible for: everything it
// submitted or resumed itself, including the children a fan-out spawned. Under
// DriveSubmittedOnly it is what the manager resyncs, so the safety net covers the
// run's own work and nothing else. It is written from the goroutine that submits
// and read from the resync loop, so it is guarded.
type drivenSet struct {
	mu   sync.Mutex
	keys map[resource.Key]struct{}
}

func (d *drivenSet) add(k resource.Key) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.keys == nil {
		d.keys = map[resource.Key]struct{}{}
	}
	d.keys[k] = struct{}{}
}

func (d *drivenSet) list() []resource.Key {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]resource.Key, 0, len(d.keys))
	for k := range d.keys {
		out = append(out, k)
	}
	return out
}

// New assembles a Runtime from cfg, building in-process foundation defaults for any
// port left nil. It registers the Goal reconciler with the manager.
func New(cfg Config) (*Runtime, error) {
	if cfg.Executor == nil || cfg.Stop == nil {
		return nil, errors.New("runtime: Executor and Stop are required")
	}

	clk := cfg.Clock
	if clk == nil {
		clk = clock.System{}
	}

	store, q := cfg.Store, cfg.Jobs
	if store == nil {
		reg := resource.NewRegistry()
		if err := resource.RegisterCoreKinds(reg); err != nil {
			return nil, err
		}
		if err := goal.RegisterKind(reg); err != nil {
			return nil, err
		}
		store = resource.NewMemory(reg, resource.WithClock(clk))
		q = jobs.NewMemory(jobs.WithClock(clk))
	}
	if q == nil {
		return nil, errors.New("runtime: Jobs is required when Store is provided")
	}

	b := cfg.Bus
	if b == nil {
		b = bus.NewMemory()
	}

	// The wake bus lets a settling child re-check its parked fan-out parent
	// promptly; the parent's recheck fallback covers a lost wake.
	ropts := []goal.Option{goal.WithWakeBus(b)}
	if cfg.PollInterval > 0 {
		ropts = append(ropts, goal.WithPollInterval(cfg.PollInterval))
	}
	if cfg.StepMaxAttempts > 0 {
		ropts = append(ropts, goal.WithStepMaxAttempts(cfg.StepMaxAttempts))
	}

	wopts := []goal.WorkerOption{goal.WithBus(b)}
	if cfg.WorkerLease > 0 {
		wopts = append(wopts, goal.WithLease(cfg.WorkerLease))
	}
	if cfg.WorkerRetryBase > 0 || cfg.WorkerRetryCeiling > 0 {
		wopts = append(wopts, goal.WithBackoff(cfg.WorkerRetryBase, cfg.WorkerRetryCeiling))
	}

	// Turning planning on is a single decision applied to both halves at once: the
	// reconciler gates a goal on planning before it builds, and the worker is given the
	// planner that runs that phase. Pairing them here means the two can never drift into
	// the state where a goal is gated on a planner that was never wired.
	if cfg.Planner != nil {
		ropts = append(ropts, goal.WithPlanning())
		wopts = append(wopts, goal.WithPlanner(cfg.Planner))
	}

	// No-progress detection lives entirely on the reconciler (it folds each step's
	// fingerprint into the goal's status and stops a stuck run), so unlike planning it is
	// a single-sided wire: the probe reads the record the worker already writes.
	if cfg.Progress != nil {
		ropts = append(ropts, goal.WithProgressProbe(cfg.Progress))
	}

	// Plan-driven fan-out is a single-sided wire too: the reconciler admits the units and
	// settles them from what their children recorded, and the worker has no part in it,
	// because a parked parent is a goal that dispatches no step at all.
	if cfg.Units != nil {
		ropts = append(ropts, goal.WithUnitSpawner(cfg.Units))
	}

	// The terms of a run are audited on the reconcile path only, for the same reason:
	// the audit is a judgement about the record the steps left, made between them.
	if cfg.Auditor != nil {
		ropts = append(ropts, goal.WithInvariantAudit(cfg.Auditor))
	}

	// Reading the refusals is single-sided as well: the waist writes them from wherever a
	// step runs, and the reconciler is where a whole run's worth of them can be read at
	// once. A step cannot make this judgement about itself, because no step is the
	// problem.
	if cfg.Refusals != nil {
		ropts = append(ropts, goal.WithRefusalProbe(cfg.Refusals))
	}

	// Close the ledger loop, both sides at once. The gate is constructed here rather
	// than injected because constructing it IS the check: NewEvidenceGate refuses to
	// return a gate that cannot demonstrate, against its own code, that it refuses a
	// claim with no evidence and admits one with fresh evidence. A broken gate therefore
	// fails composition, loudly, instead of being wired in and silently certifying every
	// claim at runtime, which is the failure the self-test was written for, finally
	// placed where it can stop a process from starting.
	if cfg.Verifier != nil && cfg.Evidence != nil {
		var gopts []goal.GateOption
		if !cfg.AllowAssertedEvidence {
			gopts = append(gopts, goal.RequireExecuted())
		}
		gate, err := goal.NewEvidenceGate(gopts...)
		if err != nil {
			return nil, fmt.Errorf("runtime: evidence gate: %w", err)
		}
		ropts = append(ropts, goal.WithLedgerGate(cfg.Evidence, gate))
		if cfg.RequireLedgerProof {
			ropts = append(ropts, goal.WithLedgerConvergence())
		}
		wopts = append(wopts, goal.WithItemVerification(cfg.Verifier, cfg.Evidence))
	}

	rec := goal.NewReconciler(store, q, clk, cfg.Stop, ropts...)
	worker := goal.NewWorker(store, q, clk, cfg.Executor, wopts...)

	driven := &drivenSet{}
	mopts := []reconcile.ManagerOption{reconcile.WithClock(clk)}
	if cfg.Resync > 0 {
		mopts = append(mopts, reconcile.WithResync(cfg.Resync))
	}
	if cfg.DriveSubmittedOnly {
		// Scope the safety net to this process's own goals rather than switching it
		// off: no pre-existing goal is adopted, but a lost change hint for a goal this
		// run submitted is still recovered on the next resync tick.
		mopts = append(mopts, reconcile.WithResyncScope(driven.list))
	}
	mgr := reconcile.NewManager(store, mopts...)
	mgr.Register(goal.Kind, rec)

	workerPoll := cfg.WorkerPoll
	if workerPoll <= 0 {
		workerPoll = DefaultWorkerPoll
	}

	return &Runtime{
		store:      store,
		jobs:       q,
		bus:        b,
		manager:    mgr,
		worker:     worker,
		clk:        clk,
		workerPoll: workerPoll,
		driven:     driven,
	}, nil
}

// Store returns the resource store the runtime drives, so callers can read goal
// status and submit related resources through the same foundation.
func (rt *Runtime) Store() resource.Store { return rt.store }

// SubmitGoal records a new Goal and enqueues it for reconciliation. An empty name
// gets a server-assigned one. The returned resource carries the assigned name and
// identity; reconciliation proceeds asynchronously once Start is running.
func (rt *Runtime) SubmitGoal(ctx context.Context, name string, spec goal.Spec) (resource.Resource, error) {
	raw, err := json.Marshal(spec)
	if err != nil {
		return resource.Resource{}, err
	}
	r := resource.Resource{APIVersion: goal.GroupVersion, Kind: goal.Kind, Spec: raw}
	if name != "" {
		r.Name = name
	} else {
		r.GenerateName = "goal-"
	}
	saved, err := rt.store.Put(ctx, r)
	if err != nil {
		return resource.Resource{}, err
	}
	rt.driven.add(saved.Key())
	rt.manager.Enqueue(saved.Key())
	return saved, nil
}

// Resume re-drives an existing goal by name: it loads the goal and enqueues it for
// reconciliation, so a run left non-terminal (parked or interrupted) is driven on
// toward completion. Unlike SubmitGoal it neither creates nor overwrites the goal,
// so the goal's recorded progress is preserved and the continuation lands on the
// same run. It returns the goal, or a store error (resource.ErrNotFound) if no goal
// of that name exists.
func (rt *Runtime) Resume(ctx context.Context, name string) (resource.Resource, error) {
	r, err := rt.store.Get(ctx, goal.Kind, resource.Scope{}, name)
	if err != nil {
		return resource.Resource{}, err
	}
	rt.driven.add(r.Key())
	rt.manager.Enqueue(r.Key())
	return r, nil
}

// Start runs the control plane until ctx is cancelled: it subscribes to step
// completion signals (waking the reconciler promptly), then runs the manager and
// the step worker concurrently, blocking until both have stopped. It returns
// ctx.Err() on shutdown.
func (rt *Runtime) Start(ctx context.Context) error {
	// Crash recovery: a previous process may have died mid-step, leaving its step job
	// leased but never completed. Under the single-instance lock no other worker is
	// live, so make any such orphan immediately claimable rather than waiting out its
	// lease. This is what lets a resumed run continue at once instead of stalling until
	// the lease lapses.
	if _, err := rt.jobs.Recover(ctx); err != nil {
		return fmt.Errorf("runtime: recover orphaned jobs: %w", err)
	}

	sub, err := rt.bus.Subscribe(ctx, goal.StepSubject, func(ctx context.Context, m bus.Message) error {
		// The signal payload is the goal's resource ID. Resolve it to a key and
		// enqueue a reconcile. A goal that has since vanished needs none; any other
		// store error is returned so the bus can surface it.
		r, err := rt.store.GetByID(ctx, string(m.Payload))
		if errors.Is(err, resource.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		rt.manager.Enqueue(r.Key())
		return nil
	})
	if err != nil {
		return err
	}
	defer func() { _ = sub.Unsubscribe() }()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); rt.manager.Start(ctx) }()
	go func() { defer wg.Done(); rt.worker.Run(ctx, rt.workerPoll) }()
	wg.Wait()
	return ctx.Err()
}

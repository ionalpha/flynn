// Package runtime wires the store, job queue, bus, reconcile manager, goal
// reconciler and step worker into one startable unit.
//
// Step execution and convergence are injected, so tests run a stub executor.
// The reconciler dispatches a step, the worker runs it and signals completion,
// and the signal re-triggers the reconciler; resync recovers a lost signal.
// Progress is recorded on the durable store and queue, so a restart resumes
// mid-goal.
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

// DefaultWorkerPoll is the idle poll interval. Both bundled queues implement
// jobs.Waker, so this only sweeps for scheduled retries, expired leases and
// enqueues from other processes.
const DefaultWorkerPoll = 50 * time.Millisecond

// Config assembles a Runtime. Executor and Stop are required; everything else
// has an in-process default.
type Config struct {
	// Executor performs one goal step. Required.
	Executor goal.StepExecutor
	// Stop decides whether a goal has converged. Required.
	Stop goal.StopEvaluator
	// Planner expands a goal's objective into its ledger before building, and
	// setting it turns planning on. The runtime wires the worker's planner and
	// the reconciler's planning gate together, so no goal is gated on a phase
	// nothing runs. Nil goes straight to building with no ledger.
	Planner goal.Planner
	// Progress fingerprints the work recorded after each build step; a goal
	// whose fingerprint is unchanged for a few steps stops with a no-progress
	// reason. Nil bounds a goal only by its step budget.
	Progress goal.ProgressProbe
	// Units runs a goal's declared unit graph as child goals in dependency
	// order, and the goal converges once every unit is proven. The executor's
	// spawn tool is unaffected. A goal with a graph and no Units stalls saying
	// so, because a skipped graph would look like a goal that never fanned out.
	Units goal.UnitSpawner
	// Auditor checks a goal's invariants after each step; a breach stops the
	// goal before the stop evaluator runs. A goal that states terms with no
	// Auditor stalls saying so, because an unaudited run finishes looking like
	// one whose terms held.
	Auditor goal.InvariantAuditor
	// SteerJudge decides whether a run's account of finishing addresses the
	// operator's redirects; an unaddressed one refuses the completion and stops
	// the goal. A steered goal with no SteerJudge stalls saying so.
	SteerJudge goal.SteerJudge
	// Halt lets an operator's kill stop the running step at dispatch, inside
	// the step. It must be the brake the executor dispatches under; any other
	// brake records the kill and stops nothing. New fails unless Halt proves a
	// halted run is refused. With nil, a kill still settles the goal, but only
	// at the next step boundary.
	Halt goal.Halter
	// Refusals reads the gates that refused this run, so a run that keeps
	// pushing on one is stopped naming it. Nil turns this off; refusals are
	// recorded on the spine either way.
	Refusals goal.RefusalProbe
	// Verifier runs a ledger item's check and Evidence stores the verdict.
	// With both set, verdicts settle the ledger through a default-FAIL gate,
	// so an item is proven only by evidence the run produced. They take effect
	// only together: a reconciler gating on evidence its worker cannot produce
	// would never converge. Either nil leaves the ledger unchecked.
	Verifier goal.ItemVerifier
	Evidence goal.Evidence
	// RequireLedgerProof lets the settled ledger decide whether a planned goal
	// is done: a completion claim over unproven items stalls the goal, naming
	// each item. It is separate from Verifier so it can be enabled once runs
	// show items being proven; enabled first, it stalls every goal.
	RequireLedgerProof bool
	// AllowAssertedEvidence accepts a verification that was asserted instead
	// of run. Off by default, so an unrunnable check fails its item. Set it
	// only when checks cannot be executed; the ledger then records claims.
	AllowAssertedEvidence bool
	// CheckDir is the directory the Verifier's checks run in. A check whose
	// command resolves to a file there (a test.bat for `test`) is refused,
	// because the agent could have written it. Name the Verifier's own
	// directory or leave it empty. It needs Verifier and Evidence.
	CheckDir string

	// Store, Jobs and Bus default to in-memory ones over Clock, with the core
	// and Goal kinds registered. A set Store needs Jobs too, and its registry
	// must include the Goal kind (goal.RegisterKind).
	Store resource.Store
	Jobs  jobs.Queue
	Bus   bus.Bus

	// Clock defaults to clock.System; a clock.Manual makes resync and backoff
	// deterministic.
	Clock clock.Timing

	// Resync overrides the manager's resync interval. Non-positive uses the
	// default; resync cannot be turned off, since it recovers lost change hints.
	Resync time.Duration
	// DriveSubmittedOnly limits the runtime to goals given through SubmitGoal
	// or Resume, including fan-out children. A one-shot command sets it so a
	// run never resumes a goal an earlier run left open. Resync still covers
	// the run's own goals. A server leaves it false to resume orphaned work.
	DriveSubmittedOnly bool
	// WorkerPoll overrides how often the step worker polls when idle.
	WorkerPoll time.Duration
	// PollInterval overrides how often the reconciler re-checks an in-flight step
	// absent a completion signal.
	PollInterval time.Duration
	// WorkerLease bounds how long a crashed worker's step waits before it is
	// re-leased (0 uses the worker default).
	WorkerLease time.Duration
	// WorkerRetryBase and WorkerRetryCeiling bound the backoff between failed
	// step attempts (0 uses the worker defaults).
	WorkerRetryBase    time.Duration
	WorkerRetryCeiling time.Duration
	// StepMaxAttempts caps retries before a step goes dead and stalls the goal
	// (0 uses the queue default).
	StepMaxAttempts int
}

// Runtime is an assembled control plane; build it with [New].
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

// drivenSet holds the goals this process submitted or resumed; under
// DriveSubmittedOnly it is the resync scope. The submitter writes it and the
// resync loop reads it.
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

// New assembles a Runtime from cfg.
func New(cfg Config) (*Runtime, error) {
	if cfg.Executor == nil || cfg.Stop == nil {
		return nil, errors.New("runtime: Executor and Stop are required")
	}

	clk := cfg.Clock
	if clk == nil {
		clk = clock.System{}
	}
	store, q, err := foundation(cfg, clk)
	if err != nil {
		return nil, err
	}
	b := cfg.Bus
	if b == nil {
		b = bus.NewMemory()
	}

	ropts, err := reconcilerOptions(cfg, b)
	if err != nil {
		return nil, err
	}
	wopts := workerOptions(cfg, b)
	ropts, wopts, err = pairedOptions(cfg, ropts, wopts)
	if err != nil {
		return nil, err
	}

	rec := goal.NewReconciler(store, q, clk, cfg.Stop, ropts...)
	worker := goal.NewWorker(store, q, clk, cfg.Executor, wopts...)

	driven := &drivenSet{}
	mgr := reconcile.NewManager(store, managerOptions(cfg, clk, driven)...)
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

func foundation(cfg Config, clk clock.Timing) (resource.Store, jobs.Queue, error) {
	store, q := cfg.Store, cfg.Jobs
	if store == nil {
		reg := resource.NewRegistry()
		if err := errors.Join(resource.RegisterCoreKinds(reg), goal.RegisterKind(reg)); err != nil {
			return nil, nil, err
		}
		store = resource.NewMemory(reg, resource.WithClock(clk))
		q = jobs.NewMemory(jobs.WithClock(clk))
	}
	if q == nil {
		return nil, nil, errors.New("runtime: Jobs is required when Store is provided")
	}
	return store, q, nil
}

// reconcilerOptions wires the ports only the reconciler uses: each judges the
// record steps leave, between steps, which no step can do about itself.
func reconcilerOptions(cfg Config, b bus.Bus) ([]goal.Option, error) {
	// A settling child wakes its parked parent; the parent's recheck covers a
	// lost wake.
	ropts := []goal.Option{goal.WithWakeBus(b)}
	if cfg.PollInterval > 0 {
		ropts = append(ropts, goal.WithPollInterval(cfg.PollInterval))
	}
	if cfg.StepMaxAttempts > 0 {
		ropts = append(ropts, goal.WithStepMaxAttempts(cfg.StepMaxAttempts))
	}

	if cfg.Progress != nil {
		ropts = append(ropts, goal.WithProgressProbe(cfg.Progress))
	}

	if cfg.Units != nil {
		ropts = append(ropts, goal.WithUnitSpawner(cfg.Units))
	}

	if cfg.Auditor != nil {
		ropts = append(ropts, goal.WithInvariantAudit(cfg.Auditor))
	}

	if cfg.SteerJudge != nil {
		ropts = append(ropts, goal.WithSteerJudge(cfg.SteerJudge))
	}

	// A kill that engages nothing is worse than none: the operator believes the
	// run stopped.
	if cfg.Halt != nil {
		if err := cfg.Halt.ProveHalts(); err != nil {
			return nil, fmt.Errorf("runtime: kill switch: %w", err)
		}
		ropts = append(ropts, goal.WithHalt(cfg.Halt))
	}

	if cfg.Refusals != nil {
		ropts = append(ropts, goal.WithRefusalProbe(cfg.Refusals))
	}
	return ropts, nil
}

func workerOptions(cfg Config, b bus.Bus) []goal.WorkerOption {
	wopts := []goal.WorkerOption{goal.WithBus(b)}
	if cfg.WorkerLease > 0 {
		wopts = append(wopts, goal.WithLease(cfg.WorkerLease))
	}
	if cfg.WorkerRetryBase > 0 || cfg.WorkerRetryCeiling > 0 {
		wopts = append(wopts, goal.WithBackoff(cfg.WorkerRetryBase, cfg.WorkerRetryCeiling))
	}
	return wopts
}

// newEvidenceGate is a variable so a test can substitute a gate that fails its
// self-test.
var newEvidenceGate = goal.NewEvidenceGate

// pairedOptions wires the features that need both the reconciler and the
// worker, so the reconciler never gates on something the worker lacks.
func pairedOptions(cfg Config, ropts []goal.Option, wopts []goal.WorkerOption) ([]goal.Option, []goal.WorkerOption, error) {
	if cfg.Planner != nil {
		ropts = append(ropts, goal.WithPlanning())
		wopts = append(wopts, goal.WithPlanner(cfg.Planner))
	}

	// The gate is built here instead of injected because NewEvidenceGate runs
	// its self-test, so a broken gate fails composition instead of certifying
	// every claim.
	if cfg.Verifier != nil && cfg.Evidence != nil {
		var gopts []goal.GateOption
		if !cfg.AllowAssertedEvidence {
			gopts = append(gopts, goal.RequireExecuted())
		}
		gate, err := newEvidenceGate(gopts...)
		if err != nil {
			return nil, nil, fmt.Errorf("runtime: evidence gate: %w", err)
		}
		ropts = append(ropts, goal.WithLedgerGate(cfg.Evidence, gate))
		if cfg.RequireLedgerProof {
			ropts = append(ropts, goal.WithLedgerConvergence())
		}
		wopts = append(wopts, goal.WithItemVerification(cfg.Verifier, cfg.Evidence))
		if cfg.CheckDir != "" {
			wopts = append(wopts, goal.WithCheckDir(cfg.CheckDir))
		}
	}
	return ropts, wopts, nil
}

func managerOptions(cfg Config, clk clock.Timing, driven *drivenSet) []reconcile.ManagerOption {
	mopts := []reconcile.ManagerOption{reconcile.WithClock(clk)}
	if cfg.Resync > 0 {
		mopts = append(mopts, reconcile.WithResync(cfg.Resync))
	}
	if cfg.DriveSubmittedOnly {
		mopts = append(mopts, reconcile.WithResyncScope(driven.list))
	}
	return mopts
}

// Store returns the store the runtime drives.
func (rt *Runtime) Store() resource.Store { return rt.store }

// SubmitGoal stores a new Goal and enqueues it; an empty name is generated.
// Reconciliation runs once Start is running.
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

// Resume enqueues an existing goal without rewriting it, so its recorded
// progress continues on the same run. A missing goal is resource.ErrNotFound.
func (rt *Runtime) Resume(ctx context.Context, name string) (resource.Resource, error) {
	r, err := rt.store.Get(ctx, goal.Kind, resource.Scope{}, name)
	if err != nil {
		return resource.Resource{}, err
	}
	rt.driven.add(r.Key())
	rt.manager.Enqueue(r.Key())
	return r, nil
}

// Start runs the manager and worker until ctx is cancelled and both stop, and
// returns ctx.Err().
func (rt *Runtime) Start(ctx context.Context) error {
	// A crashed process may have left a step leased. Under the single-instance
	// lock no other worker is live, so release it now instead of waiting out
	// the lease.
	if _, err := rt.jobs.Recover(ctx); err != nil {
		return fmt.Errorf("runtime: recover orphaned jobs: %w", err)
	}

	sub, err := rt.bus.Subscribe(ctx, goal.StepSubject, func(ctx context.Context, m bus.Message) error {
		// The payload is the goal's resource ID. A deleted goal needs nothing.
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

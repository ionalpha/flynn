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
	"sync"
	"time"

	"github.com/ionalpha/flynn/bus"
	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/jobs"
	"github.com/ionalpha/flynn/reconcile"
	"github.com/ionalpha/flynn/resource"
)

// DefaultWorkerPoll is how often the step worker polls for work when the queue is
// idle and no completion signal has woken it.
const DefaultWorkerPoll = 50 * time.Millisecond

// Config assembles a Runtime. Executor and Stop are required: they are the agent's
// behaviour, which the substrate does not supply. Everything else has a
// standalone, in-process default.
type Config struct {
	// Executor performs one goal step. Required.
	Executor goal.StepExecutor
	// Stop decides whether a goal has converged. Required.
	Stop goal.StopEvaluator

	// Store, Jobs, and Bus are the substrate ports. When Store is nil, an in-memory
	// store, queue, and bus are built over Clock with a registry holding the core
	// and Goal kinds. When Store is set, Jobs must be set too, and the registry the
	// store admits against must already include the Goal kind (goal.RegisterKind).
	Store resource.Store
	Jobs  jobs.Queue
	Bus   bus.Bus

	// Clock is the shared time source (default clock.System). A clock.Manual makes
	// resync and retry backoff deterministic in tests.
	Clock clock.Timing

	// Resync overrides the manager's safety-net interval (0 uses its default).
	Resync time.Duration
	// DriveSubmittedOnly makes the runtime drive only the goals it is explicitly
	// given (via SubmitGoal or a completion signal) and never adopt goals it finds
	// already in the store. A one-shot command sets this so starting a run does not
	// silently resume a goal an earlier run left non-terminal: each run keeps its
	// own event stream, and resuming a parked run is an explicit act. A long-lived
	// server leaves it false so the resync safety net drives every goal to
	// convergence after a crash.
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
}

// New assembles a Runtime from cfg, building in-process substrate defaults for any
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

	var ropts []goal.Option
	if cfg.PollInterval > 0 {
		ropts = append(ropts, goal.WithPollInterval(cfg.PollInterval))
	}
	if cfg.StepMaxAttempts > 0 {
		ropts = append(ropts, goal.WithStepMaxAttempts(cfg.StepMaxAttempts))
	}
	rec := goal.NewReconciler(store, q, clk, cfg.Stop, ropts...)

	wopts := []goal.WorkerOption{goal.WithBus(b)}
	if cfg.WorkerLease > 0 {
		wopts = append(wopts, goal.WithLease(cfg.WorkerLease))
	}
	if cfg.WorkerRetryBase > 0 || cfg.WorkerRetryCeiling > 0 {
		wopts = append(wopts, goal.WithBackoff(cfg.WorkerRetryBase, cfg.WorkerRetryCeiling))
	}
	worker := goal.NewWorker(store, q, clk, cfg.Executor, wopts...)

	mopts := []reconcile.ManagerOption{reconcile.WithClock(clk)}
	if cfg.Resync != 0 {
		mopts = append(mopts, reconcile.WithResync(cfg.Resync))
	}
	if cfg.DriveSubmittedOnly {
		mopts = append(mopts, reconcile.WithoutResync())
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
	}, nil
}

// Store returns the resource store the runtime drives, so callers can read goal
// status and submit related resources through the same substrate.
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
	rt.manager.Enqueue(r.Key())
	return r, nil
}

// Start runs the control plane until ctx is cancelled: it subscribes to step
// completion signals (waking the reconciler promptly), then runs the manager and
// the step worker concurrently, blocking until both have stopped. It returns
// ctx.Err() on shutdown.
func (rt *Runtime) Start(ctx context.Context) error {
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

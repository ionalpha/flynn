package orchestrate

import (
	"context"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/observe"
	"github.com/ionalpha/flynn/reconcile"
)

// DesiredState is the models that should be resident and the device-memory budget they
// share. It is read fresh on every reconcile, so a change needs no event.
type DesiredState struct {
	Models []Desired
	Budget int64
}

// Provider supplies the current desired state, read once per reconcile.
type Provider interface {
	Desired(ctx context.Context) (DesiredState, error)
}

// Server is the orchestrator's view of the running model servers. Launch and Evict must
// be idempotent so a reconcile can re-run safely. Launch takes a level, so a model that
// keeps running out of memory or wedging is relaunched smaller, down to a CPU-only run.
type Server interface {
	Resident(ctx context.Context) ([]Resident, error)
	Launch(ctx context.Context, modelID string, level LaunchLevel) error
	Evict(ctx context.Context, modelID string) error
}

// serveKey is the controller's single reconcile key. Eviction is a budget-wide decision,
// so one key drives one recomputation of the whole plan.
type serveKey struct{}

// Controller drives the resident set toward the desired state on a reconcile loop. Each
// pass schedules a plan and applies it, evictions first to free memory. An apply failure
// is a transient error, so the loop backs off and retries. Because [Schedule] is a fixed
// point, a converged set does no work and cannot thrash.
type Controller struct {
	provider Provider
	server   Server
	ctrl     *reconcile.Controller[serveKey]
	resync   time.Duration
	// classify maps a launch error to a failure kind, so recovery reacts to the cause.
	classify func(error) FailureKind
	// failures is the per-model recovery memory. Only reconcile touches it, and the work
	// queue runs that one at a time for the single key, so it needs no lock.
	failures map[string]RecoveryState
}

// Option configures a Controller.
type Option func(*Controller)

// WithResync sets how often the controller reconciles without a trigger, as a safety
// net. A value <= 0 leaves only the initial reconcile and explicit Trigger calls.
func WithResync(d time.Duration) Option {
	return func(c *Controller) { c.resync = d }
}

// WithClassifier sets how a launch error maps to a failure kind. The default treats
// every error as a crash, retried a bounded number of times and then quarantined; the
// serve layer injects one that recognizes out-of-memory and wedged loads so they
// degrade instead.
func WithClassifier(f func(error) FailureKind) Option {
	return func(c *Controller) {
		if f != nil {
			c.classify = f
		}
	}
}

// defaultClassify treats any launch error as a crash, without assuming a memory cause.
func defaultClassify(error) FailureKind { return FailureCrash }

// NewController builds a controller that reconciles server toward what provider wants,
// scheduling its retries and resync on clk.
func NewController(provider Provider, server Server, clk clock.Timing, opts ...Option) *Controller {
	c := &Controller{
		provider: provider,
		server:   server,
		resync:   15 * time.Second,
		classify: defaultClassify,
		failures: map[string]RecoveryState{},
	}
	for _, o := range opts {
		o(c)
	}
	q := reconcile.NewQueue[serveKey](clk)
	c.ctrl = reconcile.NewController("serving", q, reconcile.ReconcilerFunc[serveKey](c.reconcile))
	return c
}

// Trigger asks for a reconcile now. Triggers collapse, so a burst of changes causes one
// reconcile.
func (c *Controller) Trigger() { c.ctrl.Queue().Add(serveKey{}) }

// Run drives the loop until ctx is cancelled, with an initial reconcile, then blocks
// until every worker has drained.
func (c *Controller) Run(ctx context.Context) {
	c.Trigger()
	c.ctrl.Run(ctx)
}

// reconcile is one pass of the loop. On success it asks to be re-run after the resync
// interval, so the loop keeps converging if a trigger is lost.
func (c *Controller) reconcile(ctx context.Context, _ serveKey) (reconcile.Result, error) {
	res := reconcile.Result{RequeueAfter: c.resync}
	ds, err := c.provider.Desired(ctx)
	if err != nil {
		return res, fault.Wrap(fault.Transient, "orchestrate_desired", err)
	}
	resident, err := c.server.Resident(ctx)
	if err != nil {
		return res, fault.Wrap(fault.Transient, "orchestrate_observe", err)
	}
	c.forgetUndesired(ds.Models)
	plan := Schedule(ds.Models, resident, ds.Budget)
	if err := c.apply(ctx, plan); err != nil {
		return res, err
	}
	if len(plan.Unschedulable) > 0 {
		observe.FromContext(ctx).Log.Warn(ctx, "orchestrate: desired models do not fit the memory budget",
			observe.Int("count", len(plan.Unschedulable)))
	}
	return res, nil
}

// forgetUndesired clears the recovery memory of models no longer desired, so a model
// wanted again later is not still quarantined.
func (c *Controller) forgetUndesired(desired []Desired) {
	if len(c.failures) == 0 {
		return
	}
	wanted := make(map[string]bool, len(desired))
	for _, d := range desired {
		wanted[d.ModelID] = true
	}
	for id := range c.failures {
		if !wanted[id] {
			delete(c.failures, id)
		}
	}
}

// apply evicts before launching so freed memory is available to new models. A model's
// recovery state shapes its launch: normal, degraded after a memory or wedge failure,
// or skipped when quarantined. A failed launch is recorded so the next pass escalates,
// and a clean one clears the history. Every action is attempted, and the failures are
// collected into one transient error so a partial apply is retried.
func (c *Controller) apply(ctx context.Context, p Plan) error {
	var failed int
	var first error
	for _, id := range p.Evict {
		if err := c.server.Evict(ctx, id); err != nil {
			failed++
			if first == nil {
				first = err
			}
		}
	}
	for _, id := range p.Launch {
		level := LaunchFull
		if st, failing := c.failures[id]; failing {
			switch Recover(st) {
			case RecoverQuarantine:
				// Skip until the desired state changes; a skip is not an apply failure.
				continue
			case RecoverFallback:
				level = LaunchMinimal
			case RecoverDegrade:
				level = LaunchDegraded
			case RecoverRetry:
				level = LaunchFull
			}
		}
		if err := c.server.Launch(ctx, id, level); err != nil {
			c.recordFailure(id, err)
			failed++
			if first == nil {
				first = err
			}
			continue
		}
		delete(c.failures, id) // launched cleanly; forget any past failures
	}
	if failed > 0 {
		return fault.Wrap(fault.Transient, "orchestrate_apply", first)
	}
	return nil
}

// recordFailure classifies a launch error and advances the model's recovery state, so
// the next pass escalates.
func (c *Controller) recordFailure(id string, err error) {
	st := c.failures[id]
	st.Kind = c.classify(err)
	st.Attempts++
	c.failures[id] = st
}

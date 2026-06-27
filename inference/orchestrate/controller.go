package orchestrate

import (
	"context"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/observe"
	"github.com/ionalpha/flynn/reconcile"
)

// DesiredState is what the controller should make true: the models that should be resident
// and the device-memory budget they share. It is read fresh on every reconcile, so a change
// in what the system wants is picked up without an event.
type DesiredState struct {
	Models []Desired
	Budget int64
}

// Provider supplies the current desired state, read once per reconcile.
type Provider interface {
	Desired(ctx context.Context) (DesiredState, error)
}

// Server is the orchestrator's view of the running model servers: the resident set it can
// observe, and the launch and evict it can drive. Launch and evict must be idempotent
// (launching a running model and evicting an absent one are no-ops), which is what lets a
// reconcile re-run safely. Launch takes a degraded flag: when the recovery policy has
// decided a model keeps running out of memory or wedging, the controller asks for a smaller
// footprint instead of repeating the launch that failed.
type Server interface {
	Resident(ctx context.Context) ([]Resident, error)
	Launch(ctx context.Context, modelID string, degraded bool) error
	Evict(ctx context.Context, modelID string) error
}

// serveKey is the controller's single reconcile key. The orchestrator reconciles the whole
// resident set at once, because eviction is a global, budget-wide decision, so one key
// drives one recomputation of the entire plan.
type serveKey struct{}

// Controller drives the resident set toward the desired state on a reconcile loop. Each pass
// re-reads the desired state and the observed resident set, computes a plan with Schedule,
// and applies it: evictions first to free memory, then launches. An apply failure is reported
// as a transient error so the loop backs off and retries, which is what makes the controller
// self-healing. Because Schedule is a fixed point, a converged set produces an empty plan, so
// a steady state does no work and cannot thrash.
type Controller struct {
	provider Provider
	server   Server
	ctrl     *reconcile.Controller[serveKey]
	resync   time.Duration
	// classify maps a launch error to a failure kind, so recovery can react to the cause.
	classify func(error) FailureKind
	// failures is the per-model recovery memory consulted before a relaunch. It is only read
	// or written inside reconcile, which the work queue runs one-at-a-time for the single
	// serving key, so it needs no lock.
	failures map[string]RecoveryState
}

// Option configures a Controller.
type Option func(*Controller)

// WithResync sets how often the controller re-reconciles without a trigger, the safety net
// that re-checks desired against observed. A value <= 0 disables periodic resync, leaving
// only the initial reconcile and explicit Trigger calls.
func WithResync(d time.Duration) Option {
	return func(c *Controller) { c.resync = d }
}

// WithClassifier sets how a launch error is mapped to a failure kind, which drives the
// recovery response. Without one, every launch error is treated as a crash (retried a
// bounded number of times, then quarantined); the serve layer injects a classifier that
// recognizes out-of-memory and wedged-load failures so they degrade instead of repeating.
func WithClassifier(f func(error) FailureKind) Option {
	return func(c *Controller) {
		if f != nil {
			c.classify = f
		}
	}
}

// defaultClassify treats any launch error as a crash, the conservative default that retries
// a bounded number of times and then quarantines, without assuming a memory cause.
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

// Trigger asks for a reconcile now, for example after the desired state changes. Triggers
// collapse, so a burst of changes causes a single reconcile.
func (c *Controller) Trigger() { c.ctrl.Queue().Add(serveKey{}) }

// Run drives the loop until ctx is cancelled. It triggers an initial reconcile so the current
// desired state is applied at once, then blocks until every worker has drained.
func (c *Controller) Run(ctx context.Context) {
	c.Trigger()
	c.ctrl.Run(ctx)
}

// reconcile is one pass of the loop: read the desired state and the observed resident set,
// schedule, and apply. It asks to be re-run after the resync interval on success, so the loop
// keeps converging even if a trigger is lost.
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

// forgetUndesired clears the recovery memory of any model no longer in the desired set, so a
// model that is removed and later wanted again starts with a clean slate rather than staying
// quarantined from a previous life.
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

// apply carries out a plan, evicting before launching so freed memory is available to the new
// models. Each launch is shaped by the model's recovery state: a model with no recent failure
// launches normally, a memory or wedge failure launches degraded, and a model the policy has
// quarantined is skipped (which is not an apply failure). A launch error is classified and
// recorded so the next pass escalates; a clean launch clears the model's failure history.
// Failures are collected into one transient error so a partial apply is retried rather than
// abandoned, and every action is attempted even if an earlier one fails.
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
		degraded := false
		if st, failing := c.failures[id]; failing {
			switch Recover(st) {
			case RecoverQuarantine, RecoverFallback:
				// Stop launching this model for now. Falling back to a different model is not
				// wired yet, so it is treated as quarantine. A skip is not an apply failure.
				continue
			case RecoverDegrade:
				degraded = true
			case RecoverRetry:
				degraded = false
			}
		}
		if err := c.server.Launch(ctx, id, degraded); err != nil {
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

// recordFailure classifies a launch error and advances the model's recovery state, so the
// next pass escalates the response per the recovery ladder.
func (c *Controller) recordFailure(id string, err error) {
	st := c.failures[id]
	st.Kind = c.classify(err)
	st.Attempts++
	c.failures[id] = st
}

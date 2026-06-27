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
// reconcile re-run safely.
type Server interface {
	Resident(ctx context.Context) ([]Resident, error)
	Launch(ctx context.Context, modelID string) error
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
}

// Option configures a Controller.
type Option func(*Controller)

// WithResync sets how often the controller re-reconciles without a trigger, the safety net
// that re-checks desired against observed. A value <= 0 disables periodic resync, leaving
// only the initial reconcile and explicit Trigger calls.
func WithResync(d time.Duration) Option {
	return func(c *Controller) { c.resync = d }
}

// NewController builds a controller that reconciles server toward what provider wants,
// scheduling its retries and resync on clk.
func NewController(provider Provider, server Server, clk clock.Timing, opts ...Option) *Controller {
	c := &Controller{provider: provider, server: server, resync: 15 * time.Second}
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

// apply carries out a plan, evicting before launching so freed memory is available to the new
// models, and collecting failures into one transient error so a partial apply is retried
// rather than abandoned. Every action is attempted even if an earlier one fails, so one stuck
// model does not block the rest of the convergence.
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
		if err := c.server.Launch(ctx, id); err != nil {
			failed++
			if first == nil {
				first = err
			}
		}
	}
	if failed > 0 {
		return fault.Wrap(fault.Transient, "orchestrate_apply", first)
	}
	return nil
}

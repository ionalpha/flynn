package service

import (
	"context"
	"errors"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/reconcile"
	"github.com/ionalpha/flynn/resource"
)

// DefaultPoll is how often a running service is re-observed when the supervisor
// settles a reconcile without an error. It is a backstop on top of the manager's
// resync: a healthy workload is re-checked on this cadence so a drift the store
// never hears about is still caught.
const DefaultPoll = 60 * time.Second

// Observation is what a Driver reports about a live workload. Phase is the provider's
// own health word ("running", "failed"), or empty when the provider cannot say (it
// declares no status operation, or returned nothing recognizable). URL is the live
// address the workload currently serves at, empty when unchanged or unknown. The
// supervisor records these onto the service's status; it never interprets them beyond
// "did anything change".
type Observation struct {
	Phase string
	URL   string
}

// Driver is the provider-agnostic boundary the supervisor drives a workload through.
// The supervisor decides WHEN to act and toward WHICH desired state; the driver runs
// the one remote call for the workload's provider. A driver resolves the credential
// the service recorded from the vault itself, so no secret ever reaches the supervisor
// or the service record.
//
// Observe reads a workload's current health without changing it. Teardown removes the
// remote workload; it must be idempotent, because the supervisor may call it again if
// the record outlives a first attempt (a crash between teardown and record deletion).
type Driver interface {
	Observe(ctx context.Context, svc Service) (Observation, error)
	Teardown(ctx context.Context, svc Service) error
}

// Supervisor is the reconciler that holds a deployed Service in its desired state. It
// is level-triggered: handed only a key, it re-reads the live service every time and
// drives it toward Spec.DesiredState, so it is self-healing and crash-resumable. A
// service wanting to run is re-observed and its status refreshed; a service wanting to
// stop is torn down through its provider and its record retired. The supervisor owns
// no provider knowledge: every remote effect goes through the Driver.
type Supervisor struct {
	store *Store
	drv   Driver
	clk   clock.Timing
	poll  time.Duration
}

// SupervisorOption configures a Supervisor.
type SupervisorOption func(*Supervisor)

// WithClock sets the time source used to stamp observed status (default clock.System).
func WithClock(c clock.Timing) SupervisorOption {
	return func(s *Supervisor) {
		if c != nil {
			s.clk = c
		}
	}
}

// WithPoll sets the re-observe interval for a running service (default DefaultPoll). A
// value <= 0 disables the periodic re-observe, leaving only the manager's resync to
// re-drive the service.
func WithPoll(d time.Duration) SupervisorOption {
	return func(s *Supervisor) { s.poll = d }
}

// NewSupervisor builds a supervisor over store that effects remote changes through drv.
func NewSupervisor(store *Store, drv Driver, opts ...SupervisorOption) *Supervisor {
	s := &Supervisor{store: store, drv: drv, clk: clock.System{}, poll: DefaultPoll}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Reconcile drives one service toward its desired state. It is the Reconciler the
// reconcile manager runs for the Service kind. A vanished service settles silently; a
// store read error retries; the desired state selects the action.
func (s *Supervisor) Reconcile(ctx context.Context, key reconcile.Ref) (reconcile.Result, error) {
	svc, err := s.store.Get(ctx, key.Name)
	if errors.Is(err, ErrNotFound) {
		return reconcile.Result{}, nil // already gone; nothing to hold
	}
	if err != nil {
		return reconcile.Result{}, err // transient store error: the controller retries
	}
	switch svc.Spec.DesiredState {
	case StateStopped:
		return s.reconcileStopped(ctx, svc)
	default:
		// Running or unset both mean "keep it up": an unset desired state defaults to
		// supervised rather than abandoned.
		return s.reconcileRunning(ctx, svc)
	}
}

// reconcileRunning observes the workload and records what changed, then asks to be
// re-observed after the poll interval. The status write is skipped when nothing
// changed, so a healthy service does not churn a new resource version every poll.
func (s *Supervisor) reconcileRunning(ctx context.Context, svc Service) (reconcile.Result, error) {
	obs, err := s.drv.Observe(ctx, svc)
	if err != nil {
		return reconcile.Result{}, err
	}
	next := svc.Status
	if obs.Phase != "" {
		next.Phase = obs.Phase
	}
	if obs.URL != "" {
		next.ObservedURL = obs.URL
	}
	if next != svc.Status {
		if _, err := s.store.Put(ctx, svc.Name, svc.Spec, next); err != nil {
			return reconcile.Result{}, err
		}
	}
	return reconcile.Result{RequeueAfter: s.poll}, nil
}

// reconcileStopped tears the workload down through its provider, then retires the
// record. Teardown runs first so the record is only deleted once the remote workload is
// confirmed gone; a teardown error keeps the record so the next reconcile retries,
// which is why a driver's Teardown must be idempotent.
func (s *Supervisor) reconcileStopped(ctx context.Context, svc Service) (reconcile.Result, error) {
	if err := s.drv.Teardown(ctx, svc); err != nil {
		return reconcile.Result{}, err
	}
	if err := s.store.Delete(ctx, svc.Name); err != nil {
		return reconcile.Result{}, err
	}
	return reconcile.Result{}, nil
}

// guard: a Supervisor is the Service kind's reconciler.
var _ reconcile.Reconciler[resource.Key] = (*Supervisor)(nil)

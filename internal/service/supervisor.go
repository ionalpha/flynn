package service

import (
	"context"
	"errors"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/reconcile"
	"github.com/ionalpha/flynn/resource"
)

// DefaultPoll is how often a running service is re-observed after an error-free
// reconcile. It backs up the manager's resync, so drift the store never hears about is
// still caught.
const DefaultPoll = 60 * time.Second

// Observation is what a Driver reports about a live workload. Phase is the provider's
// health word ("running", "failed"), empty when the provider cannot say. URL is the
// live address, empty when unchanged or unknown. The supervisor records both on the
// service's status and reads them only to see whether anything changed.
type Observation struct {
	Phase string
	URL   string
}

// Driver is the provider-agnostic interface the supervisor drives a workload through.
// The supervisor decides when to act and toward which state; the driver makes the one
// remote call. A driver resolves the service's credential from the vault itself, so no
// secret reaches the supervisor or the record.
//
// Observe reads a workload's health without changing it. Teardown removes the remote
// workload and must be idempotent, because the supervisor calls it again when the
// record outlives a first attempt (a crash between teardown and record deletion).
type Driver interface {
	Observe(ctx context.Context, svc Service) (Observation, error)
	Teardown(ctx context.Context, svc Service) error
}

// Supervisor is the reconciler that holds a deployed Service in its desired state. It
// is level-triggered: given only a key, it re-reads the service each time and drives it
// toward Spec.DesiredState, so it heals itself and resumes after a crash. A running
// service is re-observed; a stopped one is torn down and its record retired. Every
// remote effect goes through the Driver.
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
// value of zero or less leaves only the manager's resync to re-drive the service.
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

// Reconcile drives one service toward its desired state for the reconcile manager. A
// vanished service settles; a store read error retries.
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
		// An unset desired state means running, so a service is supervised by default.
		return s.reconcileRunning(ctx, svc)
	}
}

// reconcileRunning observes the workload, records what changed, and asks to be
// re-observed after the poll interval. Nothing is written when nothing changed, so a
// healthy service does not mint a resource version every poll.
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

// reconcileStopped tears the workload down, then retires the record, so the record goes
// only once the remote workload is gone. A teardown error keeps the record for the next
// reconcile to retry, which is why Teardown must be idempotent.
func (s *Supervisor) reconcileStopped(ctx context.Context, svc Service) (reconcile.Result, error) {
	if err := s.drv.Teardown(ctx, svc); err != nil {
		return reconcile.Result{}, err
	}
	if err := s.store.Delete(ctx, svc.Name); err != nil {
		return reconcile.Result{}, err
	}
	return reconcile.Result{}, nil
}

// Supervisor is the Service kind's reconciler.
var _ reconcile.Reconciler[resource.Key] = (*Supervisor)(nil)

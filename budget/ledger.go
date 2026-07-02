package budget

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/resource"
)

// Ledger reads and writes run budgets on the resource store. It is the durable
// home of a run's spend: concurrent charges converge on one record under the
// store's optimistic concurrency, so a shared pool stays correct across a fan-out
// and across a crash.
type Ledger struct {
	store resource.Store
}

// NewLedger returns a Ledger backed by store.
func NewLedger(store resource.Store) *Ledger { return &Ledger{store: store} }

// Open creates (or returns the existing) budget for run id in scope, capped by
// limits. Pass owners to bind the budget to the run that owns it (an
// OwnerReference to the run's goal), so the budget is garbage-collected when the
// run ends rather than outliving it.
func (l *Ledger) Open(ctx context.Context, id string, scope resource.Scope, limits Limits, owners ...resource.OwnerReference) (resource.Resource, error) {
	if existing, err := l.store.Get(ctx, Kind, scope, id); err == nil {
		return existing, nil
	} else if !errors.Is(err, resource.ErrNotFound) {
		return resource.Resource{}, err
	}
	spec, err := json.Marshal(Spec{Limits: limits})
	if err != nil {
		return resource.Resource{}, err
	}
	r := resource.Resource{APIVersion: GroupVersion, Kind: Kind, Name: id, Scope: scope, Spec: spec}
	r.OwnerReferences = owners
	return l.store.Put(ctx, r)
}

// Available reports whether the run identified by id still has budget: true when
// no budget is bound (unlimited), and true until the recorded spend reaches a set
// limit. It is the pre-execution check the dispatch waist gates an action on.
func (l *Ledger) Available(ctx context.Context, id string, scope resource.Scope) (bool, error) {
	r, err := l.store.Get(ctx, Kind, scope, id)
	if errors.Is(err, resource.ErrNotFound) {
		return true, nil // no budget bound: unlimited
	}
	if err != nil {
		return false, err
	}
	spec, err := DecodeSpec(r)
	if err != nil {
		return false, err
	}
	status, err := DecodeStatus(r)
	if err != nil {
		return false, err
	}
	return !spec.Limits.Exceeded(status.Spent), nil
}

// Reserve atomically holds est against the run's pool before an action runs: the
// reserve half of reserve-before-dispatch. It admits (returns true) while the pool
// still has budget left, where "left" means the committed total (spent plus
// already-reserved) has not reached a set limit, and records the reservation; it
// refuses (false) once the pool is fully committed. Because the check and the
// reservation are one compare-and-set, concurrent actions sharing a pool admit
// against one consistent view rather than each reading an under-budget snapshot and
// overshooting together. A run with no budget bound is unlimited: always admits,
// records nothing. With an upper-bound estimate the ceiling cannot be exceeded;
// with a smaller estimate the overshoot is bounded by the in-flight estimates.
func (l *Ledger) Reserve(ctx context.Context, id string, scope resource.Scope, est Spent) (bool, error) {
	admitted := false
	_, err := resource.Update(ctx, l.store, Kind, scope, id, func(r *resource.Resource) error {
		spec, err := DecodeSpec(*r)
		if err != nil {
			return err
		}
		status, err := DecodeStatus(*r)
		if err != nil {
			return err
		}
		if spec.Limits.Exceeded(status.committed()) {
			admitted = false
			return resource.ErrSkipUpdate // pool fully committed: refuse before the action runs
		}
		admitted = true
		status.Reserved = status.Reserved.plus(est)
		enc, err := status.Encode()
		if err != nil {
			return err
		}
		r.Status = enc
		return nil
	})
	switch {
	case errors.Is(err, resource.ErrNotFound):
		return true, nil // no budget bound: unlimited
	case errors.Is(err, resource.ErrConflict):
		return false, fault.New(fault.Transient, "budget_reserve_contention",
			"budget reserve gave up after repeated write conflicts")
	case err != nil:
		return false, err
	}
	return admitted, nil
}

// Release returns a reservation to the pool without spending it, for an action that
// was admitted but did not run (rejected downstream, cancelled). It floors the
// reserved total at zero so a doubled release under a race cannot drive it negative.
func (l *Ledger) Release(ctx context.Context, id string, scope resource.Scope, est Spent) error {
	if est.IsZero() {
		return nil
	}
	return l.update(ctx, id, scope, func(s *Status) {
		s.Reserved = s.Reserved.minusFloored(est)
	})
}

// Settle converts a reservation into actual spend once an action finishes: it
// releases est and charges the metered actual in one atomic write, so the pool
// never briefly double-counts (reserved and spent) nor under-counts (released
// before charged). A zero est with a real metering behaves like Charge.
func (l *Ledger) Settle(ctx context.Context, id string, scope resource.Scope, est Spent, m dispatch.Metering) error {
	return l.update(ctx, id, scope, func(s *Status) {
		s.Reserved = s.Reserved.minusFloored(est)
		s.Spent.Tokens += int64(m.Tokens)
		s.Spent.Cost += m.Cost
	})
}

// update applies fn to the run's budget status under the store's shared
// conflict-retry policy, so charge, release, and settle all converge on a shared
// pool. A run with no budget bound is a no-op.
func (l *Ledger) update(ctx context.Context, id string, scope resource.Scope, fn func(*Status)) error {
	_, err := resource.Update(ctx, l.store, Kind, scope, id, func(r *resource.Resource) error {
		status, err := DecodeStatus(*r)
		if err != nil {
			return err
		}
		fn(&status)
		enc, err := status.Encode()
		if err != nil {
			return err
		}
		r.Status = enc
		return nil
	})
	switch {
	case errors.Is(err, resource.ErrNotFound):
		return nil // no budget bound: unlimited, nothing to record
	case errors.Is(err, resource.ErrConflict):
		return fault.New(fault.Transient, "budget_update_contention",
			"budget update gave up after repeated write conflicts")
	}
	return err
}

// Charge adds m to the run's recorded spend, retrying under optimistic
// concurrency so concurrent charges against a shared pool all land. A run with no
// budget bound is a no-op (unlimited). Charging more than the limit is allowed:
// the limit is enforced before an action runs (see Available), and the actual
// cost is only known after, so the recorded spend is the truth and can settle
// slightly past the ceiling.
func (l *Ledger) Charge(ctx context.Context, id string, scope resource.Scope, m dispatch.Metering) error {
	if m.Tokens == 0 && m.Cost == 0 {
		return nil
	}
	return l.update(ctx, id, scope, func(s *Status) {
		s.Spent.Tokens += int64(m.Tokens)
		s.Spent.Cost += m.Cost
	})
}

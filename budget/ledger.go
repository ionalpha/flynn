package budget

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/resource"
)

// Ledger keeps run budgets on the resource store. Writes use optimistic concurrency, so
// a shared pool stays correct across a fan-out and a crash.
type Ledger struct {
	store     resource.Store
	extendCap Limits // zero allows no extension
}

// NewLedger returns a Ledger with no extension cap.
func NewLedger(store resource.Store) *Ledger { return &Ledger{store: store} }

// Open creates run id's budget, or returns the existing one unchanged. Owners (the
// run's goal) let the budget be garbage-collected with the run.
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

// Available reports whether recorded spend is under every set limit. An unbound run is
// always available.
func (l *Ledger) Available(ctx context.Context, id string, scope resource.Scope) (bool, error) {
	r, err := l.store.Get(ctx, Kind, scope, id)
	if errors.Is(err, resource.ErrNotFound) {
		return true, nil
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

// Reserve adds est to Reserved and admits while spent plus reserved is under every set
// limit. The check and the add are one compare-and-set, so concurrent actions admit
// against one view. With an upper-bound est the ceiling holds; a smaller one bounds
// the overshoot by the in-flight estimates. An unbound run always admits.
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
			return resource.ErrSkipUpdate
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
		return true, nil
	case errors.Is(err, resource.ErrConflict):
		return false, fault.New(fault.Transient, "budget_reserve_contention",
			"budget reserve gave up after repeated write conflicts")
	case err != nil:
		return false, err
	}
	return admitted, nil
}

// Release returns est unspent, for an admitted action that did not run.
func (l *Ledger) Release(ctx context.Context, id string, scope resource.Scope, est Spent) error {
	if est.IsZero() {
		return nil
	}
	return l.update(ctx, id, scope, func(s *Status) {
		s.Reserved = s.Reserved.minusFloored(est)
	})
}

// Settle releases est and charges m in one write, so the pool never counts the action
// twice or not at all.
func (l *Ledger) Settle(ctx context.Context, id string, scope resource.Scope, est Spent, m dispatch.Metering) error {
	tier, _ := TierFromContext(ctx)
	return l.update(ctx, id, scope, func(s *Status) {
		s.Reserved = s.Reserved.minusFloored(est)
		s.Spent.Tokens += int64(m.Tokens)
		s.Spent.Cost += m.Cost
		s.attribute(tier, int64(m.Tokens), m.Cost)
	})
}

// Spend returns the run's recorded status, for a caller that needs the numbers. An
// unbound run returns the zero Status rather than an error.
func (l *Ledger) Spend(ctx context.Context, id string, scope resource.Scope) (Status, error) {
	r, err := l.store.Get(ctx, Kind, scope, id)
	if errors.Is(err, resource.ErrNotFound) {
		return Status{}, nil
	}
	if err != nil {
		return Status{}, err
	}
	return DecodeStatus(r)
}

// update applies fn under the store's conflict retry. An unbound run is a no-op.
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
		return nil
	case errors.Is(err, resource.ErrConflict):
		return fault.New(fault.Transient, "budget_update_contention",
			"budget update gave up after repeated write conflicts")
	}
	return err
}

// Charge adds m to the recorded spend. It may pass the ceiling, because the limit is
// checked before an action and its cost is known only after.
func (l *Ledger) Charge(ctx context.Context, id string, scope resource.Scope, m dispatch.Metering) error {
	if m.Tokens == 0 && m.Cost == 0 {
		return nil
	}
	tier, _ := TierFromContext(ctx)
	return l.update(ctx, id, scope, func(s *Status) {
		s.Spent.Tokens += int64(m.Tokens)
		s.Spent.Cost += m.Cost
		s.attribute(tier, int64(m.Tokens), m.Cost)
	})
}

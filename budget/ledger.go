package budget

import (
	"context"
	"encoding/json"
	"errors"
	"runtime"

	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/resource"
)

// maxChargeRetries bounds the optimistic-concurrency retry loop a charge makes
// when concurrent runs write the same pool. Every conflict means another writer
// won and committed, so each round makes global progress; a charge yields between
// attempts to let the winner settle before re-reading. The bound is high so a
// charge effectively never loses under realistic fan-out, and only a pathological
// thundering herd would exhaust it (surfaced as a transient error, never a silently
// dropped charge).
const maxChargeRetries = 1000

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
	for range maxChargeRetries {
		r, err := l.store.Get(ctx, Kind, scope, id)
		if errors.Is(err, resource.ErrNotFound) {
			return nil // no budget bound: unlimited, nothing to record
		}
		if err != nil {
			return err
		}
		status, err := DecodeStatus(r)
		if err != nil {
			return err
		}
		status.Spent.Tokens += int64(m.Tokens)
		status.Spent.Cost += m.Cost
		enc, err := status.Encode()
		if err != nil {
			return err
		}
		r.Status = enc
		// Put with the read SyncVersion is a compare-and-set: a concurrent charge
		// that wrote in between fails with ErrConflict, so yield to let the winner
		// settle, then re-read and retry against the new version.
		if _, err := l.store.Put(ctx, r); errors.Is(err, resource.ErrConflict) {
			runtime.Gosched()
			continue
		} else if err != nil {
			return err
		}
		return nil
	}
	return fault.New(fault.Transient, "budget_charge_contention",
		"budget charge gave up after repeated write conflicts")
}

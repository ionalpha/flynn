// Package testkit is shared test infrastructure for the agent: deterministic
// fault injection and invariant checks that make rigorous tests cheap to write.
//
// Chaos engineering falls out of the ports architecture — wrap any port
// (dispatch.Handler, dispatch.EventSink, …) with a FaultPlan and assert the
// system degrades and recovers cleanly. Combined with clock.Manual and seeded
// inputs, the faults are deterministic, so a failing run reproduces exactly.
package testkit

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/resource"
)

// FaultPlan decides, deterministically, when to inject a fault. Each wrapped
// call advances a counter; the plan returns an error to inject on that call, or
// nil to pass through. Safe for concurrent use.
type FaultPlan struct {
	mu   sync.Mutex
	n    int
	fire func(call int) error
}

func newPlan(fire func(call int) error) *FaultPlan { return &FaultPlan{fire: fire} }

// next advances the call counter and returns the fault for this call, if any.
func (p *FaultPlan) next() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.n++
	return p.fire(p.n)
}

// FailOnCall injects err only on the call with the given 1-based index.
func FailOnCall(call int, err error) *FaultPlan {
	return newPlan(func(c int) error {
		if c == call {
			return err
		}
		return nil
	})
}

// FailFirst injects err on the first k calls, then passes through. Models a
// flaky dependency that recovers — exercises retry and degrade paths.
func FailFirst(k int, err error) *FaultPlan {
	return newPlan(func(c int) error {
		if c <= k {
			return err
		}
		return nil
	})
}

// FailEvery injects err on every nth call (n <= 0 never fails).
func FailEvery(n int, err error) *FaultPlan {
	return newPlan(func(c int) error {
		if n > 0 && c%n == 0 {
			return err
		}
		return nil
	})
}

// Always injects err on every call.
func Always(err error) *FaultPlan {
	return newPlan(func(int) error { return err })
}

// FaultyHandler wraps a dispatch.Handler, injecting plan's faults before
// delegating. A nil inner handler returns a zero Result when not failing, so a
// plan alone is enough to drive a handler in tests.
func FaultyHandler(inner dispatch.Handler, plan *FaultPlan) dispatch.Handler {
	return dispatch.HandlerFunc(func(ctx context.Context, a dispatch.Action) (dispatch.Result, error) {
		if err := plan.next(); err != nil {
			return dispatch.Result{}, err
		}
		if inner == nil {
			return dispatch.Result{}, nil
		}
		return inner.Handle(ctx, a)
	})
}

// FaultySink wraps a dispatch.EventSink, injecting plan's faults before
// delegating — used to prove that event-sink failures never break a dispatch.
// A nil inner sink discards events when not failing.
func FaultySink(inner dispatch.EventSink, plan *FaultPlan) dispatch.EventSink {
	return sinkFunc(func(ctx context.Context, e dispatch.Event) error {
		if err := plan.next(); err != nil {
			return err
		}
		if inner == nil {
			return nil
		}
		return inner.Append(ctx, e)
	})
}

type sinkFunc func(context.Context, dispatch.Event) error

func (f sinkFunc) Append(ctx context.Context, e dispatch.Event) error { return f(ctx, e) }

var _ dispatch.EventSink = sinkFunc(nil)

// FaultyExecutor wraps a goal.StepExecutor, injecting plan's faults before
// delegating, so a flaky model or tool call (a step that fails a few times then
// succeeds) is modelled deterministically. A nil inner executor performs no work
// when not failing, so a plan alone is enough to drive a goal through its retry
// and recovery path. The checkpoint of a faulting call is dropped: a failed step
// makes no progress.
func FaultyExecutor(inner goal.StepExecutor, plan *FaultPlan) goal.StepExecutor {
	return execFunc(func(ctx context.Context, r resource.Resource) (json.RawMessage, error) {
		if err := plan.next(); err != nil {
			return nil, err
		}
		if inner == nil {
			return nil, nil
		}
		return inner.Execute(ctx, r)
	})
}

type execFunc func(context.Context, resource.Resource) (json.RawMessage, error)

func (f execFunc) Execute(ctx context.Context, r resource.Resource) (json.RawMessage, error) {
	return f(ctx, r)
}

var _ goal.StepExecutor = execFunc(nil)

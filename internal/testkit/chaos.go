// Package testkit is shared test infrastructure for the agent: deterministic
// fault injection and invariant checks that make rigorous tests cheap to write.
//
// Chaos engineering falls out of the ports architecture — wrap a unit of work or
// any port (dispatch work, dispatch.EventSink, …) with a FaultPlan and assert the
// system degrades and recovers cleanly. Combined with clock.Manual and seeded
// inputs, the faults are deterministic, so a failing run reproduces exactly.
package testkit

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/ionalpha/flynn/bus"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/integrations/request"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/spine"
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

// FaultyWork wraps a unit of dispatch work, injecting plan's faults before
// delegating. A nil inner returns zero Metering when not failing, so a plan alone
// is enough to drive a Dispatcher.Govern call through its retry and recovery path.
func FaultyWork(inner func(context.Context) (dispatch.Metering, error), plan *FaultPlan) func(context.Context) (dispatch.Metering, error) {
	return func(ctx context.Context) (dispatch.Metering, error) {
		if err := plan.next(); err != nil {
			return dispatch.Metering{}, err
		}
		if inner == nil {
			return dispatch.Metering{}, nil
		}
		return inner(ctx)
	}
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

// FaultyModel wraps an llm.Model, injecting plan's faults on Generate before
// delegating. It models a flaky provider (a transient API error) so a test can
// drive the conversation loop's retry path, where the fault lands AFTER a turn has
// already been announced. That is the case the well-formedness invariant guards:
// a retried turn must not duplicate its turn.started on the event stream.
func FaultyModel(inner llm.Model, plan *FaultPlan) llm.Model {
	return modelFunc(func(ctx context.Context, req llm.Request) (llm.Response, error) {
		if err := plan.next(); err != nil {
			return llm.Response{}, err
		}
		return inner.Generate(ctx, req)
	})
}

type modelFunc func(context.Context, llm.Request) (llm.Response, error)

func (f modelFunc) Generate(ctx context.Context, req llm.Request) (llm.Response, error) {
	return f(ctx, req)
}

var _ llm.Model = modelFunc(nil)

// FaultyBus wraps a bus.Bus, injecting plan's faults on Publish before delegating;
// Subscribe and Close pass through. It proves a publisher tolerates a flaky or
// dead signal bus: a failed or dropped publish must never corrupt the durable
// record a subscriber ultimately reads. A nil plan never faults. A nil inner bus
// models a fully dead bus: Publish is a silent no-op and Subscribe returns an
// inert subscription that never delivers, so a consumer must make progress some
// other way (a poll floor).
func FaultyBus(inner bus.Bus, plan *FaultPlan) bus.Bus {
	return &faultyBus{inner: inner, plan: plan}
}

type faultyBus struct {
	inner bus.Bus
	plan  *FaultPlan
}

func (b *faultyBus) Publish(ctx context.Context, m bus.Message) error {
	if b.plan != nil {
		if err := b.plan.next(); err != nil {
			return err
		}
	}
	if b.inner == nil {
		return nil
	}
	return b.inner.Publish(ctx, m)
}

func (b *faultyBus) Subscribe(ctx context.Context, pattern string, h bus.Handler) (bus.Subscription, error) {
	if b.inner == nil {
		return inertSub(pattern), nil
	}
	return b.inner.Subscribe(ctx, pattern, h)
}

func (b *faultyBus) Close() error {
	if b.inner == nil {
		return nil
	}
	return b.inner.Close()
}

// inertSub is a live-looking but never-firing subscription, for a dead inner bus.
type inertSub string

func (s inertSub) Subject() string  { return string(s) }
func (inertSub) Unsubscribe() error { return nil }

var _ bus.Bus = (*faultyBus)(nil)

// FaultyDoer wraps a request.Doer, injecting plan's faults before delegating, so a
// flaky network (a few transient failures then recovery, or a hard outage) is
// modelled deterministically against the shared HTTP transport. The injected error
// flows through the transport's classification, so a fault.Transient drives the
// retry path and a fault.Terminal proves a non-retryable failure is not replayed. A
// nil inner doer returns a minimal 200 response when not faulting, so a plan alone
// is enough to drive the transport through its retry and recovery path.
func FaultyDoer(inner request.Doer, plan *FaultPlan) request.Doer {
	return doerFunc(func(r *http.Request) (*http.Response, error) {
		if err := plan.next(); err != nil {
			return nil, err
		}
		if inner == nil {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader("")),
				Header:     http.Header{},
			}, nil
		}
		return inner.Do(r)
	})
}

type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

var _ request.Doer = doerFunc(nil)

// FaultyLog wraps a spine.Log, injecting plan's faults on Append before
// delegating; Read passes through. It models a flaky durable log: an append that
// fails records nothing and assigns no Seq, so the stream stays gap-free (a
// dropped event simply never existed) rather than leaving a hole. A nil plan never
// faults; a nil inner log returns a zero Event on a non-faulting append and reads
// back nothing.
func FaultyLog(inner spine.Log, plan *FaultPlan) spine.Log {
	return &faultyLog{inner: inner, plan: plan}
}

type faultyLog struct {
	inner spine.Log
	plan  *FaultPlan
}

func (l *faultyLog) Append(ctx context.Context, in spine.AppendInput) (spine.Event, error) {
	if l.plan != nil {
		if err := l.plan.next(); err != nil {
			return spine.Event{}, err
		}
	}
	if l.inner == nil {
		return spine.Event{}, nil
	}
	return l.inner.Append(ctx, in)
}

func (l *faultyLog) Read(ctx context.Context, q spine.Query) ([]spine.Event, error) {
	if l.inner == nil {
		return nil, nil
	}
	return l.inner.Read(ctx, q)
}

// SaveSnapshot and LatestSnapshot pass through: the fault plan targets appends (the
// write path under test), and a snapshot is a derived cache, so injecting faults
// into it would test nothing the append faults do not already cover.
func (l *faultyLog) SaveSnapshot(ctx context.Context, s spine.Snapshot) error {
	if l.inner == nil {
		return nil
	}
	return l.inner.SaveSnapshot(ctx, s)
}

func (l *faultyLog) LatestSnapshot(ctx context.Context, stream string, upToSeq int64) (spine.Snapshot, bool, error) {
	if l.inner == nil {
		return spine.Snapshot{}, false, nil
	}
	return l.inner.LatestSnapshot(ctx, stream, upToSeq)
}

var _ spine.Log = (*faultyLog)(nil)

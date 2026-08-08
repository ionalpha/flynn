package distil

import (
	"context"
	"sync"

	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/memory/consolidate"
)

// Action is the dispatch action name a distillation runs under, so the model
// call that turns a series into a lesson is admitted, traced and charged under a
// stable, greppable name rather than reaching a provider on a side channel.
// Consolidation is offline work, which is exactly the kind that goes unnoticed
// when it is ungoverned: nobody is watching a nightly sweep spend.
const Action = "memory.distil"

// GovernedDistiller routes a distillation through the dispatch waist. It wraps
// any consolidate.Distiller; the series and the lesson stay inside the closure
// and never reach dispatch, which sees an action name, a scope and what the call
// cost.
type GovernedDistiller struct {
	inner      consolidate.Distiller
	dispatcher *dispatch.Dispatcher
}

// NewGoverned wraps inner so its distillation runs through a dispatcher built
// from opts. Pass the same admitter, event sink and observability the rest of
// the install uses (dispatch.WithAdmitter / WithEventSink / WithObservability)
// so a consolidation sweep shares their governance and spine; with no options
// the dispatcher applies standalone defaults and the distillation is recorded
// but ungoverned.
func NewGoverned(inner consolidate.Distiller, opts ...dispatch.Option) *GovernedDistiller {
	return &GovernedDistiller{inner: inner, dispatcher: dispatch.New(opts...)}
}

var _ consolidate.Distiller = (*GovernedDistiller)(nil)

// Distil governs the distillation as a scoped action and returns the inner
// lesson.
//
// Admission declining is not a failure: the distiller never runs, the zero
// Lesson comes back with no error, and the pass reads that as declined and
// leaves the series intact for a run that is admitted. A cancelled context is a
// hard error, and an error from the inner distiller propagates, so a broken
// distiller stays visible instead of looking like a series nobody could learn
// from.
func (g *GovernedDistiller) Distil(ctx context.Context, in consolidate.Series) (consolidate.Lesson, error) {
	var (
		lesson consolidate.Lesson
		ran    bool
	)
	err := g.dispatcher.Govern(ctx, dispatch.Action{Name: Action, Scope: in.Scope},
		func(ctx context.Context) (dispatch.Metering, error) {
			ran = true
			ctx, used := withUsage(ctx)
			var derr error
			lesson, derr = g.inner.Distil(ctx, in)
			return dispatch.Metering{Tokens: used.tokens()}, derr
		})
	if err != nil {
		if !ran { // admission or a hook declined before the distiller ran
			if ctx.Err() != nil {
				return consolidate.Lesson{}, ctx.Err()
			}
			return consolidate.Lesson{}, nil
		}
		return consolidate.Lesson{}, err // the inner distiller failed; surface it
	}
	return lesson, nil
}

// usage collects what a distillation spent, so the governed wrapper can report
// it as metering without the Distiller port having to carry a token count it
// only means for one implementation.
//
// It travels on the context rather than on the type because the two halves are
// deliberately separable: NewGoverned takes any Distiller, and one that is not
// this package's simply meters zero rather than being refused. Each Govern call
// installs its own collector, so concurrent distillations do not share one.
type usage struct {
	mu sync.Mutex
	n  int
}

type usageKey struct{}

func withUsage(ctx context.Context) (context.Context, *usage) {
	u := &usage{}
	return context.WithValue(ctx, usageKey{}, u), u
}

func (u *usage) tokens() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.n
}

// reportUsage adds one model call's tokens to the collector on ctx, if there is
// one. Cache reads and writes count: they are tokens the call was billed for,
// and a charge that omitted them would under-report exactly the workload caching
// is meant to make cheap.
func reportUsage(ctx context.Context, u llm.Usage) {
	c, ok := ctx.Value(usageKey{}).(*usage)
	if !ok {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n += u.InputTokens + u.OutputTokens + u.CacheReadTokens + u.CacheWriteTokens
}

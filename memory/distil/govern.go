package distil

import (
	"context"
	"sync"

	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/memory/consolidate"
	"github.com/ionalpha/flynn/usage"
)

// Action is the dispatch action a distillation runs under, so its model calls are
// admitted, traced and charged under a stable name. A nightly sweep has nobody
// watching it spend.
const Action = "memory.distil"

// GovernedDistiller runs any consolidate.Distiller through dispatch. Dispatch sees
// the action name, scope and cost; the series and lesson never reach it.
type GovernedDistiller struct {
	inner      consolidate.Distiller
	dispatcher *dispatch.Dispatcher
}

// NewGoverned wraps inner in a dispatcher built from opts. Pass the install's
// dispatch.WithAdmitter, WithEventSink and WithObservability so a sweep shares its
// governance. With no options the distillation is recorded but ungoverned.
func NewGoverned(inner consolidate.Distiller, opts ...dispatch.Option) *GovernedDistiller {
	return &GovernedDistiller{inner: inner, dispatcher: dispatch.New(opts...)}
}

var _ consolidate.Distiller = (*GovernedDistiller)(nil)

// Distil governs the distillation as a scoped action. A refused admission returns
// the zero Lesson, which the pass reads as declined, leaving the series for a later
// run. A cancelled context and an inner error are returned, so a broken distiller
// does not look like an unlearnable series.
func (g *GovernedDistiller) Distil(ctx context.Context, in consolidate.Series) (consolidate.Lesson, error) {
	var (
		lesson consolidate.Lesson
		ran    bool
	)
	err := g.dispatcher.Govern(ctx, dispatch.Action{Name: Action, Scope: in.Scope},
		func(ctx context.Context) (dispatch.Metering, error) {
			ran = true
			ctx, used := withTally(ctx)
			var derr error
			lesson, derr = g.inner.Distil(ctx, in)
			return dispatch.Metering{Tokens: used.metered()}, derr
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

// tally collects a distillation's tokens for metering, so the Distiller port need
// not carry a count. It rides the context because [NewGoverned] takes any Distiller,
// and a foreign one meters zero. Each call gets its own collector, so concurrent
// distillations do not share one.
type tally struct {
	mu     sync.Mutex
	tokens usage.Tokens
}

type tallyKey struct{}

func withTally(ctx context.Context) (context.Context, *tally) {
	t := &tally{}
	return context.WithValue(ctx, tallyKey{}, t), t
}

// metered is the collected tokens as dispatch meters them: [usage.Tokens.Total],
// so a cache read is not counted again on top of the input it is part of.
func (t *tally) metered() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return int(t.tokens.Total())
}

// reportUsage adds one call's tokens to ctx's collector, if any.
func reportUsage(ctx context.Context, u llm.Usage) {
	c, ok := ctx.Value(tallyKey{}).(*tally)
	if !ok {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tokens = c.tokens.Add(usage.FromLLM(u))
}

package learn

import (
	"context"

	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/state"
)

// VerifyAction is the dispatch action name a skill check runs under, so a
// verification appears on the event spine and in traces under a stable, greppable
// name alongside the tools the agent invokes.
const VerifyAction = "learn.verify"

// DistillAction is the dispatch action name a distillation runs under, so the model
// call that turns a finished run into lessons is admitted, traced, and recorded on
// the spine under a stable name rather than reaching the model on a side channel.
const DistillAction = "learn.distill"

// GovernedVerifier routes each skill check through the dispatch waist before it
// runs, so a verification is admitted, traced, and recorded on the event spine
// exactly like any tool the agent invokes. Executing a model-proposed command is
// itself an action with consequences; sending it through the same chokepoint means
// one capability gate, audit trail, and replay path cover it, rather than letting
// verification be a side channel that bypasses governance.
type GovernedVerifier struct {
	inner      Verifier
	dispatcher *dispatch.Dispatcher
}

// NewGovernedVerifier wraps inner so its checks run through a dispatcher built from
// opts. Pass the same admitter, event sink, and observability the rest of the agent
// uses (dispatch.WithAdmitter / WithEventSink / WithObservability) so verifications
// share its governance and spine; with no options the dispatcher applies standalone
// defaults and the check is recorded but ungoverned.
func NewGovernedVerifier(inner Verifier, opts ...dispatch.Option) *GovernedVerifier {
	return &GovernedVerifier{inner: inner, dispatcher: dispatch.New(opts...)}
}

var _ Verifier = (*GovernedVerifier)(nil)

// Verify governs the check as a scoped action and returns the inner verdict. The
// lesson and verdict stay in this closure; the dispatcher brackets the run without
// seeing them. A cancelled context is a hard error, matching the inner verifier.
// Any other failure (admission rejected the check, or the inner verifier failed for
// a non-cancel reason) means the check did not run to a clean verdict, so the skill
// is reported unproven rather than broken: governance can decline a verification
// without that being mistaken for evidence the skill is wrong.
func (g *GovernedVerifier) Verify(ctx context.Context, l Lesson, scope state.Scope) (Verdict, error) {
	var v Verdict
	err := g.dispatcher.Govern(ctx, dispatch.Action{Name: VerifyAction, Scope: scope},
		func(ctx context.Context) (dispatch.Metering, error) {
			var verr error
			v, verr = g.inner.Verify(ctx, l, scope)
			return dispatch.Metering{}, verr
		})
	if err != nil {
		if ctx.Err() != nil {
			return Verdict{}, ctx.Err()
		}
		return Verdict{Detail: "verification not admitted: " + err.Error()}, nil
	}
	return v, nil
}

// GovernedDistiller routes a Distiller's model call through the dispatch waist, so
// summarising a finished run into skills and memory is admitted, traced, and
// recorded like every other action rather than reaching the model on a side
// channel. It wraps any Distiller; the lessons stay in this closure and never reach
// dispatch.
type GovernedDistiller struct {
	inner      Distiller
	dispatcher *dispatch.Dispatcher
}

// NewGovernedDistiller wraps inner so its distillation runs through a dispatcher
// built from opts. Pass the same admitter, event sink, and observability the rest
// of the run uses; with no options the dispatcher applies standalone defaults and
// the distillation is recorded but ungoverned.
func NewGovernedDistiller(inner Distiller, opts ...dispatch.Option) *GovernedDistiller {
	return &GovernedDistiller{inner: inner, dispatcher: dispatch.New(opts...)}
}

var _ Distiller = (*GovernedDistiller)(nil)

// Distill governs the distillation as a scoped action and returns the inner
// lessons. If admission declines the distillation it never runs, so nothing is
// captured and no error is raised (governance opting out is not a failure); a
// cancelled context is a hard error. An error from the inner distiller itself
// propagates, so a broken distiller stays visible rather than silently dropping
// knowledge.
func (g *GovernedDistiller) Distill(ctx context.Context, o Outcome) ([]Lesson, error) {
	var (
		lessons []Lesson
		ran     bool
	)
	err := g.dispatcher.Govern(ctx, dispatch.Action{Name: DistillAction, Scope: o.Scope},
		func(ctx context.Context) (dispatch.Metering, error) {
			ran = true
			var derr error
			lessons, derr = g.inner.Distill(ctx, o)
			return dispatch.Metering{}, derr
		})
	if err != nil {
		if !ran { // admission or a hook declined before the distiller ran
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, nil
		}
		return nil, err // the inner distiller failed; surface it
	}
	return lessons, nil
}

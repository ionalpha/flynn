package learn

import (
	"context"

	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/state"
)

// VerifyAction is the dispatch action name of a skill check.
const VerifyAction = "learn.verify"

// DistillAction is the dispatch action name of a distillation.
const DistillAction = "learn.distill"

// GovernedVerifier runs each skill check through dispatch, so a model-proposed
// command gets the same admission, trace and event record as a tool call.
type GovernedVerifier struct {
	inner      Verifier
	dispatcher *dispatch.Dispatcher
}

// NewGovernedVerifier wraps inner in a dispatcher built from opts. Pass the agent's
// own admitter, event sink and observability; with no options the check is recorded
// but ungoverned.
func NewGovernedVerifier(inner Verifier, opts ...dispatch.Option) *GovernedVerifier {
	return &GovernedVerifier{inner: inner, dispatcher: dispatch.New(opts...)}
}

var _ Verifier = (*GovernedVerifier)(nil)

// Verify returns the inner verdict; the dispatcher never sees the lesson. A
// cancelled context is an error. Any other failure returns a verdict with Ran false,
// so a declined check marks the skill unverified and does not drop it.
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

// GovernedDistiller runs a Distiller's model call through dispatch. The lessons
// never reach the dispatcher.
type GovernedDistiller struct {
	inner      Distiller
	dispatcher *dispatch.Dispatcher
}

// NewGovernedDistiller wraps inner in a dispatcher built from opts, as
// [NewGovernedVerifier] does.
func NewGovernedDistiller(inner Distiller, opts ...dispatch.Option) *GovernedDistiller {
	return &GovernedDistiller{inner: inner, dispatcher: dispatch.New(opts...)}
}

var _ Distiller = (*GovernedDistiller)(nil)

// Distill returns the inner lessons. A declined distillation returns nothing and no
// error. A cancelled context, or an error from the inner distiller, is returned.
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
		if !ran {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, nil
		}
		return nil, err
	}
	return lessons, nil
}

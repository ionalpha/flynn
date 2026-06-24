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

// GovernedVerifier routes each skill check through the dispatch waist before it
// runs, so a verification is admitted, traced, and recorded on the event spine
// exactly like any tool the agent invokes. Executing a model-proposed command is
// itself an action with consequences; sending it through the same chokepoint means
// one capability gate, audit trail, and replay path cover it, rather than letting
// verification be a side channel that bypasses governance.
type GovernedVerifier struct {
	dispatcher *dispatch.Dispatcher
}

// NewGovernedVerifier wraps inner so its checks run through a dispatcher built from
// opts. Pass the same admitter, event sink, and observability the rest of the agent
// uses (dispatch.WithAdmitter / WithEventSink / WithObservability) so verifications
// share its governance and spine; with no options the dispatcher applies standalone
// defaults and the check is recorded but ungoverned.
func NewGovernedVerifier(inner Verifier, opts ...dispatch.Option) *GovernedVerifier {
	return &GovernedVerifier{dispatcher: dispatch.New(verifyHandler{inner}, opts...)}
}

var _ Verifier = (*GovernedVerifier)(nil)

// Verify dispatches l's check as a scoped action and returns the inner verdict. A
// cancelled context is a hard error, matching the inner verifier. Any other
// dispatch failure (admission rejected the check, or the handler failed for a
// non-cancel reason) means the check did not run, so the skill is reported unproven
// rather than broken: governance can decline a verification without that being
// mistaken for evidence the skill is wrong.
func (g *GovernedVerifier) Verify(ctx context.Context, l Lesson, scope state.Scope) (Verdict, error) {
	res, err := g.dispatcher.Dispatch(ctx, dispatch.Action{
		Name:  VerifyAction,
		Input: map[string]any{"check": l.Check, "kind": string(l.Kind), lessonKey: l},
		Scope: scope,
	})
	if err != nil {
		if ctx.Err() != nil {
			return Verdict{}, ctx.Err()
		}
		return Verdict{Detail: "verification not admitted: " + err.Error()}, nil
	}
	v, _ := res.Output[verdictKey].(Verdict)
	return v, nil
}

// verifyHandler is the dispatch.Handler that performs a verification: it runs the
// wrapped verifier and carries the verdict back through the result. It is the single
// place a check executes, so the waist's tracing and events bracket the real work.
type verifyHandler struct{ inner Verifier }

func (h verifyHandler) Handle(ctx context.Context, a dispatch.Action) (dispatch.Result, error) {
	l, _ := a.Input[lessonKey].(Lesson)
	v, err := h.inner.Verify(ctx, l, a.Scope)
	if err != nil {
		return dispatch.Result{}, err
	}
	return dispatch.Result{Output: map[string]any{verdictKey: v}}, nil
}

// Keys for the in-process action payload. The lesson and verdict pass as Go values
// through the dispatcher (an in-process call, not a serialized tool boundary), so
// the full check and result reach the handler and return intact.
const (
	lessonKey  = "lesson"
	verdictKey = "verdict"
)

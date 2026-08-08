// Package refusal implements goal.RefusalProbe over a run's durable record: it reads the
// run's own event stream and returns every action the dispatch waist refused, with the
// rule that refused it. The goal reconciler reads those together and stops a run that kept
// pushing on one gate.
//
// It reads the record and never the run's account of itself. A run that works around a
// gate reports each step as fine, which is what makes the trajectory invisible from the
// inside; the refusals are the part of the history the run does not get to narrate.
//
// It lives in its own package for the reason the progress probe does: it reads the session
// projection of the spine, and session already depends on mission, so the same read from
// mission would be a cycle. It depends on goal (the port), session (the reader) and spine
// (the log), and nothing depends on it but the composition that wires it.
package refusal

import (
	"context"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/session"
	"github.com/ionalpha/flynn/spine"
)

// SpineProbe is the goal.RefusalProbe reading a run's recorded refusals off the spine.
type SpineProbe struct {
	log spine.Log
}

var _ goal.RefusalProbe = (*SpineProbe)(nil)

// NewSpineProbe builds a refusal probe reading run streams from log. The stream to read is
// the goal's own id, taken from the resource on each call, so one probe serves a fan-out's
// children as well as its root.
func NewSpineProbe(log spine.Log) *SpineProbe { return &SpineProbe{log: log} }

// Refusals returns every action the waist refused for this goal, in the order it refused
// them.
//
// A refusal that names no rule is returned as it was recorded rather than dropped or given
// a rule here. The reconciler is the one that decides what an unattributable refusal is
// worth, and a probe that quietly filtered the record would be answering that question
// where nobody can see it.
//
// A failed read is transient: a record that could not be reached for a moment must not
// read as a run with nothing against it, which is the direction this must never fail in.
func (p *SpineProbe) Refusals(ctx context.Context, r resource.Resource) ([]goal.Refusal, error) {
	if p.log == nil {
		return nil, fault.New(fault.Terminal, "refusal_no_log",
			"refusal: no spine log wired to read the run's refusals from")
	}
	events, err := session.History(ctx, p.log, r.Name)
	if err != nil {
		return nil, fault.Wrap(fault.Transient, "refusal_history_read", err)
	}
	var out []goal.Refusal
	for _, e := range events {
		if e.Kind != session.KindActionRejected {
			continue
		}
		out = append(out, goal.Refusal{Rule: e.Code, Action: e.Action})
	}
	return out, nil
}

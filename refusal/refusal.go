// Package refusal implements goal.RefusalProbe: it reads a run's event stream and returns
// every action the dispatch waist refused, with the rule that refused it, so the goal
// reconciler can stop a run that keeps pushing on one gate.
//
// It reads the durable record because a run that works around a gate reports each step
// as fine; the refusals are the history the run cannot narrate. It is its own package
// because it reads the session projection, and session imports mission.
package refusal

import (
	"context"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/session"
	"github.com/ionalpha/flynn/spine"
)

// SpineProbe reads a run's recorded refusals off the spine.
type SpineProbe struct {
	log spine.Log
}

var _ goal.RefusalProbe = (*SpineProbe)(nil)

// NewSpineProbe builds a probe over log. The stream is the goal's id, taken from the
// resource on each call, so one probe serves a fan-out's children and its root.
func NewSpineProbe(log spine.Log) *SpineProbe { return &SpineProbe{log: log} }

// Refusals returns every action the waist refused for this goal, in order. A refusal
// with no rule is returned as recorded, because the reconciler decides what an
// unattributable refusal is worth. A failed read is transient, so an unreachable record
// never reads as a run with nothing against it.
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

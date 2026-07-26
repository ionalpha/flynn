package rubric

import (
	"context"

	"github.com/ionalpha/flynn/spine"
)

const (
	// EvIssue is the event type recorded for one confessed issue.
	EvIssue = "rubric.issue"
	// EvAxis is the event type recorded for one graded axis.
	EvAxis = "rubric.axis"
	// EvSummary is the event type recorded for a rubric's overall verdict.
	EvSummary = "rubric.summary"
)

// Record appends an assessment to the log under stream: one EvIssue event per confessed
// issue, one EvAxis event per graded axis, then one EvSummary carrying the overall score
// and pass/fail. Every event is stamped with the rubric's fingerprint, so the record
// says which grader produced the verdict — the provenance a re-grade needs to tell a
// newer grader's verdict from the one it supersedes. A verdict becomes durable,
// auditable, and replayable on the spine like any other effect, so a run can be re-graded
// by folding the stream when the grader improves. The events are written on the runtime's
// authority, so their actor is system.
func Record(ctx context.Context, log spine.Log, stream string, a Assessment) error {
	for _, is := range a.Issues {
		if _, err := log.Append(ctx, spine.AppendInput{
			Stream: stream,
			Type:   EvIssue,
			Actor:  spine.ActorSystem,
			Payload: map[string]any{
				"rubric":      a.Rubric,
				"fingerprint": a.Fingerprint,
				"axis":        is.Axis,
				"severity":    is.Severity,
				"detail":      is.Detail,
			},
		}); err != nil {
			return err
		}
	}
	for i, ax := range a.Axes {
		if _, err := log.Append(ctx, spine.AppendInput{
			Stream: stream,
			Type:   EvAxis,
			Actor:  spine.ActorSystem,
			Payload: map[string]any{
				"rubric":      a.Rubric,
				"fingerprint": a.Fingerprint,
				"index":       i,
				"axis":        ax.Axis,
				"weight":      ax.Weight,
				"score":       ax.Score,
				"reason":      ax.Reason,
				"scored":      ax.Scored,
			},
		}); err != nil {
			return err
		}
	}
	_, err := log.Append(ctx, spine.AppendInput{
		Stream: stream,
		Type:   EvSummary,
		Actor:  spine.ActorSystem,
		Payload: map[string]any{
			"rubric":      a.Rubric,
			"fingerprint": a.Fingerprint,
			"overall":     a.Overall,
			"passed":      a.Passed,
		},
	})
	return err
}

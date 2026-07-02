package grade

import (
	"context"

	"github.com/ionalpha/flynn/spine"
)

const (
	// EvMilestone is the event type recorded for one graded rung.
	EvMilestone = "grade.milestone"
	// EvSummary is the event type recorded for a ladder's overall partial-credit
	// score.
	EvSummary = "grade.summary"
)

// Record appends a grade to the log under stream: one EvMilestone event per rung in
// ladder order, then one EvSummary event carrying the score. A grade becomes durable,
// auditable, and replayable on the substrate like any other effect, so a run can be
// re-graded by folding the stream and an improved grader can be compared against the
// one that produced these events. The events are written on the runtime's authority,
// so their actor is system.
func Record(ctx context.Context, log spine.Log, stream string, g Grade) error {
	for i, r := range g.Rungs {
		_, err := log.Append(ctx, spine.AppendInput{
			Stream: stream,
			Type:   EvMilestone,
			Actor:  spine.ActorSystem,
			Payload: map[string]any{
				"ladder":  g.Ladder,
				"index":   i,
				"name":    r.Name,
				"weight":  r.Weight,
				"passed":  r.Passed,
				"reached": r.Reached,
				"detail":  r.Detail,
			},
		})
		if err != nil {
			return err
		}
	}
	_, err := log.Append(ctx, spine.AppendInput{
		Stream: stream,
		Type:   EvSummary,
		Actor:  spine.ActorSystem,
		Payload: map[string]any{
			"ladder":   g.Ladder,
			"attained": g.Attained,
			"total":    g.Total,
			"score":    g.Score,
			"reached":  g.Reached,
		},
	})
	return err
}

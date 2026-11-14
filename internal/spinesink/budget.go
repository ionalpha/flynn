package spinesink

import (
	"context"

	"github.com/ionalpha/flynn/budget"
	"github.com/ionalpha/flynn/spine"
)

// BudgetExtendedEventType is the event type a raised budget ceiling is recorded under on
// a run's stream.
const BudgetExtendedEventType = "budget.extended"

// BudgetSink records a budget extension onto a run's spine stream, attributed to the
// person who asked. It implements [budget.Recorder].
//
// The actor is [spine.ActorHuman] and the principal is the person's, because the event
// exists to answer who let this run spend more. [budget.Ledger.Extend] appends it after
// the ceiling is raised and before the goal is reopened, so a run that is working on a
// raised ceiling always has this event behind it.
type BudgetSink struct {
	log    spine.Log
	stream string
}

// NewBudget returns a BudgetSink that writes to log on the given stream.
func NewBudget(log spine.Log, stream string) *BudgetSink {
	return &BudgetSink{log: log, stream: stream}
}

// RecordExtended appends one budget.extended event carrying the ceiling before and
// after, what this extension added, the total added so far and the cap on that total.
func (s *BudgetSink) RecordExtended(ctx context.Context, e budget.Extended) error {
	_, err := s.log.Append(ctx, spine.AppendInput{
		Stream:    s.stream,
		Type:      BudgetExtendedEventType,
		Actor:     spine.ActorHuman,
		Principal: e.Principal,
		Payload: map[string]any{
			"run":         e.Run,
			"reason":      e.Reason,
			"fromTokens":  e.From.Tokens,
			"fromCost":    e.From.Cost,
			"toTokens":    e.To.Tokens,
			"toCost":      e.To.Cost,
			"addedTokens": e.Added.Tokens,
			"addedCost":   e.Added.Cost,
			"totalTokens": e.Total.Tokens,
			"totalCost":   e.Total.Cost,
			"capTokens":   e.Cap.Tokens,
			"capCost":     e.Cap.Cost,
		},
	})
	return err
}

var _ budget.Recorder = (*BudgetSink)(nil)

package spinesink_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/budget"
	"github.com/ionalpha/flynn/internal/spinesink"
	"github.com/ionalpha/flynn/spine"
)

// TestBudgetSinkRecordsWhoRaisedTheCeilingAndBy checks the event answers the audit
// question: it is a person's act, attributed to that person, and carries the ceiling
// before and after, what this extension added, the running total and the cap.
func TestBudgetSinkRecordsWhoRaisedTheCeilingAndBy(t *testing.T) {
	ctx := context.Background()
	log := spine.NewMemoryLog()
	sink := spinesink.NewBudget(log, "run-1")

	err := sink.RecordExtended(ctx, budget.Extended{
		Run: "run-1", Principal: "ada", Reason: "nearly there",
		From:  budget.Limits{Tokens: 100, Cost: 1},
		To:    budget.Limits{Tokens: 250, Cost: 2.5},
		Added: budget.Limits{Tokens: 150, Cost: 1.5},
		Total: budget.Limits{Tokens: 150, Cost: 1.5},
		Cap:   budget.Limits{Tokens: 500, Cost: 5},
	})
	if err != nil {
		t.Fatal(err)
	}
	events, err := log.Read(ctx, spine.Query{Stream: "run-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	e := events[0]
	if e.Type != spinesink.BudgetExtendedEventType || spinesink.BudgetExtendedEventType != "budget.extended" {
		t.Errorf("type = %q", e.Type)
	}
	if e.Actor != spine.ActorHuman {
		t.Errorf("actor = %q, want human: a model never extends its own budget", e.Actor)
	}
	if e.Principal != "ada" {
		t.Errorf("principal = %q, want the person who asked", e.Principal)
	}
	raw, err := json.Marshal(e.Payload)
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	for _, want := range []string{`"run":"run-1"`, `"reason":"nearly there"`, `"fromTokens":100`, `"toTokens":250`, `"addedTokens":150`, `"totalTokens":150`, `"capTokens":500`, `"toCost":2.5`, `"capCost":5`} {
		if !strings.Contains(got, want) {
			t.Errorf("payload %s is missing %s", got, want)
		}
	}
}

// failingLog refuses every append.
type failingLog struct {
	spine.Log
	err error
}

func (f failingLog) Append(context.Context, spine.AppendInput) (spine.Event, error) {
	return spine.Event{}, f.err
}

// TestBudgetSinkReportsAnAppendFailure: a lost append is an error the caller sees, so
// Extend does not reopen a run whose extension nobody can later find.
func TestBudgetSinkReportsAnAppendFailure(t *testing.T) {
	boom := errors.New("disk full")
	sink := spinesink.NewBudget(failingLog{err: boom}, "run-1")
	if err := sink.RecordExtended(context.Background(), budget.Extended{Principal: "ada"}); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the append failure", err)
	}
}

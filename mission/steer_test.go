package mission

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/llm/llmtest"
	"github.com/ionalpha/flynn/resource"
)

func steerRedirect() goal.Steer {
	return goal.Steer{ID: "use-events-table", Instruction: "you are writing to sessions; write to events instead"}
}

// steerGoal builds the resource one turn runs against: a goal under one redirect, with the
// status carrying whatever has been recorded about it.
func steerGoal(t *testing.T, status goal.Status) resource.Resource {
	t.Helper()
	spec, err := json.Marshal(goal.Spec{
		Objective:     "add the audit trail",
		StopCondition: "the trail is written",
		Steers:        []goal.Steer{steerRedirect()},
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := status.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return resource.Resource{APIVersion: goal.GroupVersion, Kind: goal.Kind, Name: "g", Spec: spec, Status: raw}
}

// TestExecutorDeliversAnOutstandingRedirect: the operator's redirect reaches the model as
// part of the conversation, with the rule it will be held to stated alongside it.
func TestExecutorDeliversAnOutstandingRedirect(t *testing.T) {
	model := llmtest.NewScripted(llmtest.SayText("ok"))
	exec := NewExecutor(model)
	var status goal.Status
	status.SyncSteers([]goal.Steer{steerRedirect()})

	if _, err := exec.Execute(context.Background(), steerGoal(t, status)); err != nil {
		t.Fatalf("execute: %v", err)
	}

	reqs := model.Requests()
	if len(reqs) == 0 {
		t.Fatal("model was not called")
	}
	if !requestHasText(reqs[0], steerRedirect().Instruction) {
		t.Fatalf("the redirect was not delivered to the model: %+v", reqs[0].Messages)
	}
	if !requestHasText(reqs[0], "state for each one what you did about it") {
		t.Fatalf("the discharge rule was not delivered with it: %+v", reqs[0].Messages)
	}
}

// TestARedirectRidesEveryTurnItSurvives is what makes surviving a reseed a property of the
// redirect rather than of how the transcript happened to be trimmed. The turn here opens on
// a history that has no trace of the redirect in it, which is what a compacted or pruned
// transcript looks like, and the redirect is delivered again from the durable record.
func TestARedirectRidesEveryTurnItSurvives(t *testing.T) {
	model := llmtest.NewScripted(llmtest.SayText("ok"))
	exec := NewExecutor(model)
	var status goal.Status
	status.SyncSteers([]goal.Steer{steerRedirect()})
	reseeded, err := encodeCheckpoint(checkpoint{
		Messages: []llm.Message{llm.Text(llm.RoleUser, "add the audit trail"), llm.Text(llm.RoleAssistant, "working on it")},
		Turns:    9,
	})
	if err != nil {
		t.Fatal(err)
	}
	status.Checkpoint = reseeded

	if _, err := exec.Execute(context.Background(), steerGoal(t, status)); err != nil {
		t.Fatalf("execute: %v", err)
	}

	reqs := model.Requests()
	if len(reqs) == 0 {
		t.Fatal("model was not called")
	}
	if !requestHasText(reqs[0], steerRedirect().Instruction) {
		t.Fatalf("the redirect did not survive a transcript that never carried it: %+v", reqs[0].Messages)
	}
}

// TestADischargedRedirectStopsBeingDelivered: the obligation is over, so the turn is not
// spent re-litigating it.
func TestADischargedRedirectStopsBeingDelivered(t *testing.T) {
	model := llmtest.NewScripted(llmtest.SayText("ok"))
	exec := NewExecutor(model)
	var status goal.Status
	status.SyncSteers([]goal.Steer{steerRedirect()})
	status.RecordAcknowledgements([]goal.Steer{steerRedirect()},
		[]goal.Acknowledgement{{ID: steerRedirect().ID, How: "moved the writer"}}, steerNow)

	if _, err := exec.Execute(context.Background(), steerGoal(t, status)); err != nil {
		t.Fatalf("execute: %v", err)
	}

	reqs := model.Requests()
	if len(reqs) == 0 {
		t.Fatal("model was not called")
	}
	if requestHasText(reqs[0], steerRedirect().Instruction) {
		t.Fatalf("a discharged redirect was still being delivered: %+v", reqs[0].Messages)
	}
}

// steerNow is the fixed instant these tests stamp an acknowledgement at. The wall clock is
// not what any of them are about.
var steerNow = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

package spinesink_test

import (
	"context"
	"testing"

	"github.com/ionalpha/flynn/approval"
	"github.com/ionalpha/flynn/internal/spinesink"
	"github.com/ionalpha/flynn/spine"
	"github.com/ionalpha/flynn/state"
)

// TestApprovalSinkRecordsBothVerdicts: a grant and a denial both land on the run's stream
// as approval.decision events, carrying what was asked for, who authorized it and why it
// was refused. Recording only the grants would leave a record of successes where an
// auditor needs a record of decisions.
func TestApprovalSinkRecordsBothVerdicts(t *testing.T) {
	ctx := context.Background()
	log := spine.NewMemoryLog()
	sink := spinesink.NewApproval(log, "run-1")

	granted := approval.Decision{
		Envelope: approval.Envelope{
			Action:    "deploy",
			Scope:     state.Scope{Instance: "inst", Project: "proj"},
			Principal: "run-1",
			Detail:    "prod",
			Host:      "box",
		},
		KeyIDs:  []string{"operator"},
		Granted: true,
	}
	if err := sink.Record(ctx, granted); err != nil {
		t.Fatalf("record grant: %v", err)
	}
	denied := approval.Decision{
		Envelope: approval.Envelope{Action: "shell", Host: "box"},
		Reason:   "no approvals presented",
	}
	if err := sink.Record(ctx, denied); err != nil {
		t.Fatalf("record denial: %v", err)
	}

	events, err := log.Read(ctx, spine.Query{Stream: "run-1"})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("got %d events, want both decisions", len(events))
	}
	for _, e := range events {
		if e.Type != "approval.decision" {
			t.Fatalf("event type = %q, want approval.decision", e.Type)
		}
		// The decision is a person's, even when it is the gate that writes it down.
		if e.Actor != spine.ActorHuman {
			t.Fatalf("actor = %q, want human", e.Actor)
		}
	}

	first := events[0].Payload
	if first["verdict"] != "granted" {
		t.Fatalf("verdict = %v, want granted", first["verdict"])
	}
	if first["action"] != "deploy" || first["detail"] != "prod" {
		t.Fatalf("the grant does not name what was authorized: %v", first)
	}
	// The scope reads as the path a person expects rather than a struct dump, and it
	// stops at the first empty level so an unset workspace is not a trailing separator.
	if first["scope"] != "inst/proj" {
		t.Fatalf("scope = %v, want inst/proj", first["scope"])
	}
	if first["approvers"] != "operator" {
		t.Fatalf("the grant does not name who authorized it: %v", first["approvers"])
	}

	second := events[1].Payload
	if second["verdict"] != "denied" {
		t.Fatalf("verdict = %v, want denied", second["verdict"])
	}
	if second["reason"] != "no approvals presented" {
		t.Fatalf("the denial does not say why: %v", second["reason"])
	}
	if second["scope"] != "" {
		t.Fatalf("the empty scope rendered as %q, want the empty string", second["scope"])
	}
}

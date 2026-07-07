package main

import (
	"context"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/state"
)

// TestShellRecallShowsInTranscript proves that when a turn pulls learned skills or
// memory into context, the session surfaces a recall line so the recall is visible
// rather than a silent prompt addition.
func TestShellRecallShowsInTranscript(t *testing.T) {
	host, ui := newHostForTest(t, constModel{text: "ok"})
	if _, err := host.s.store.Skills().Upsert(context.Background(), state.Skill{
		Slug: "deploy", Name: "Deploy", Body: "how to deploy the service", Tags: []string{"verified"},
	}); err != nil {
		t.Fatal(err)
	}

	host.submit("deploy the service now", nil)
	waitIdle(t, host)

	if !strings.Contains(ui.transcript(), "recalled") {
		t.Fatalf("recall was not surfaced in the transcript:\n%s", ui.transcript())
	}
}

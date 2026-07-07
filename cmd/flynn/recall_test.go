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

	tr := ui.transcript()
	if !strings.Contains(tr, "recalled") || !strings.Contains(tr, "Deploy") {
		t.Fatalf("recall did not name the recalled item in the transcript:\n%s", tr)
	}
}

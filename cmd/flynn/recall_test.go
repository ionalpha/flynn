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

// TestRecallOffersTheDescriptionWhole is the disclosure boundary. What goes into the
// prompt is what a skill says about itself at discovery, entire; the procedure is
// left for skill_read. Before this, a 3,000-word skill went in as its first 240
// characters, which is neither the offer nor the procedure.
func TestRecallOffersTheDescriptionWhole(t *testing.T) {
	ctx := context.Background()
	st := memStore(t)
	const description = "Reduce a change to the smallest diff that still does the job. " +
		"Reach for it before asking anyone to read a deploy branch, and again after the review comes back, " +
		"because the second pass is where the leftovers from the first one show up and a reviewer who has " +
		"already read the change will not see them a second time."
	body := "Read the diff as a reviewer would.\n" + strings.Repeat("Then remove what the change does not need. ", 40)
	if _, err := st.Skills().Upsert(ctx, state.Skill{
		Slug: "tidy-diff", Name: "Tidy the diff", Description: description, Body: body,
	}); err != nil {
		t.Fatal(err)
	}

	block, _, _ := recallContext(ctx, st.Skills(), st.Memory(), "tidy the deploy diff")
	if !strings.Contains(block, description) {
		t.Errorf("the offer does not carry the whole description:\n%s", block)
	}
	if strings.Contains(block, "Then remove what the change does not need") {
		t.Errorf("the offer carries the body, which is what skill_read is for:\n%s", block)
	}
	if !strings.Contains(block, "skill_read") {
		t.Errorf("the offer does not tell the model how to load a skill it wants:\n%s", block)
	}
}

// TestRecallFallsBackToTheBodyWithoutADescription keeps the skills minted before the
// description field existed recallable. The fallback is the old behaviour and it is
// a poor offer, which is the argument for capture writing a description of its own;
// it is not an argument for those skills going dark in the meantime.
func TestRecallFallsBackToTheBodyWithoutADescription(t *testing.T) {
	ctx := context.Background()
	st := memStore(t)
	if _, err := st.Skills().Upsert(ctx, state.Skill{
		Slug: "deploy-flow", Name: "Deploy flow", Body: "run the deploy script then verify",
	}); err != nil {
		t.Fatal(err)
	}

	block, _, _ := recallContext(ctx, st.Skills(), st.Memory(), "deploy the service")
	if !strings.Contains(block, "run the deploy script then verify") {
		t.Errorf("a skill with no description was offered nothing at all:\n%s", block)
	}
}

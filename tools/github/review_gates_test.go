package github_test

// What the reviewer refuses to do. Two gates: APPROVE is off unless explicitly enabled
// and never applies to the reviewer's own pull request, and a finding with no concrete
// failure scenario is a nitpick that never reaches the API. A reviewer that approves by
// default is a merge gate on every repository it is installed on; one that posts
// unevidenced findings is noise a maintainer learns to skip.

import (
	"errors"
	"testing"

	"github.com/ionalpha/flynn/tools/github"
)

// APPROVE must be refused unless explicitly enabled. A reviewer that approves by
// default silently becomes a merge gate on every repository it is installed on.
func TestSubmitReviewApproveRefusedByDefault(t *testing.T) {
	hub := newFakeHub(t)
	set := newSet(t, hub, nil)

	_, err := invoke(t, toolNamed(t, set, "github_submit_review"), `{"event":"APPROVE","conclusion":"Nothing blocking."}`)
	if !errors.Is(err, github.ErrApproveNotEnabled) {
		t.Fatalf("want ErrApproveNotEnabled, got %v", err)
	}
	if v := hub.submittedBody.Load(); v != nil {
		t.Fatalf("a review was submitted despite the refusal: %v", v)
	}
}

func TestSubmitReviewApproveAllowedWhenEnabled(t *testing.T) {
	hub := newFakeHub(t)
	set := newSet(t, hub, func(c *github.Config) { c.AllowApprove = true })

	if _, err := invoke(t, toolNamed(t, set, "github_submit_review"), `{"event":"APPROVE","conclusion":"Nothing blocking."}`); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if got := hub.submittedBody.Load(); got != "APPROVE" {
		t.Fatalf("event = %v, want APPROVE", got)
	}
}

// Even with approval enabled, the reviewer must not approve a pull request it
// authored. The author is read live from the API, so a caller cannot misreport it.
func TestSubmitReviewRefusesSelfApproval(t *testing.T) {
	hub := newFakeHub(t)
	hub.prAuthor = "reviewer[bot]"
	set := newSet(t, hub, func(c *github.Config) { c.AllowApprove = true })

	_, err := invoke(t, toolNamed(t, set, "github_submit_review"), `{"event":"APPROVE","conclusion":"Nothing blocking."}`)
	if !errors.Is(err, github.ErrSelfApproval) {
		t.Fatalf("want ErrSelfApproval, got %v", err)
	}
	if v := hub.submittedBody.Load(); v != nil {
		t.Fatalf("a self-approval was submitted: %v", v)
	}
}

// REQUEST_CHANGES needs no opt-in: blocking is always permitted, only approving is
// gated. A reviewer that cannot say no is not a reviewer.
func TestSubmitReviewRequestChangesNeedsNoOptIn(t *testing.T) {
	hub := newFakeHub(t)
	hub.prAuthor = "reviewer[bot]" // even on its own PR, blocking is fine
	set := newSet(t, hub, nil)

	if _, err := invoke(t, toolNamed(t, set, "github_submit_review"), `{"event":"REQUEST_CHANGES","conclusion":"Findings need addressing."}`); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if got := hub.submittedBody.Load(); got != "REQUEST_CHANGES" {
		t.Fatalf("event = %v, want REQUEST_CHANGES", got)
	}
}

func TestSubmitReviewRejectsUnknownEvent(t *testing.T) {
	hub := newFakeHub(t)
	set := newSet(t, hub, nil)

	if _, err := invoke(t, toolNamed(t, set, "github_submit_review"), `{"event":"LGTM"}`); err == nil {
		t.Fatal("want an error for an unknown event")
	}
}

// A finding with no concrete failure scenario is a nitpick and is refused before
// anything is posted.
func TestFindingWithoutFailureScenarioIsRefused(t *testing.T) {
	hub := newFakeHub(t)
	set := newSet(t, hub, nil)
	tool := toolNamed(t, set, "github_comment")

	cases := map[string]string{
		"no failure": `{"findings":[{"path":"a.go","line":1,"rule":"r","summary":"s","failure":"  "}]}`,
		"no summary": `{"findings":[{"path":"a.go","line":1,"rule":"r","summary":"","failure":"f"}]}`,
		"no rule":    `{"findings":[{"path":"a.go","line":1,"rule":"","summary":"s","failure":"f"}]}`,
		"no line":    `{"findings":[{"path":"a.go","line":0,"rule":"r","summary":"s","failure":"f"}]}`,
		"no path":    `{"findings":[{"path":"","line":1,"rule":"r","summary":"s","failure":"f"}]}`,
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := invoke(t, tool, in); err == nil {
				t.Fatal("want a refusal")
			}
		})
	}
	if got := hub.created.Load(); got != 0 {
		t.Fatalf("created = %d, want 0: an invalid finding reached the API", got)
	}
}

// A batch is validated whole: one bad finding posts none of them, so a pull
// request is never left half-reviewed.
func TestInvalidFindingInBatchPostsNothing(t *testing.T) {
	hub := newFakeHub(t)
	set := newSet(t, hub, nil)
	tool := toolNamed(t, set, "github_comment")

	in := `{"findings":[
      {"path":"a.go","line":1,"rule":"r","summary":"s","failure":"f"},
      {"path":"b.go","line":2,"rule":"r","summary":"s","failure":""}]}`
	if _, err := invoke(t, tool, in); err == nil {
		t.Fatal("want a refusal")
	}
	if got := hub.created.Load(); got != 0 {
		t.Fatalf("created = %d, want 0: the valid half of a bad batch was posted", got)
	}
}

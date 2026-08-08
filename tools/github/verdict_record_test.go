package github_test

// What a submitted verdict records. The verdict closes the review's record: a submitted
// one ends it, a failed submission keeps the findings so a retry still has them, and a
// reposted finding is counted once. Tool input cannot redirect a verdict to a different
// pull request than the one under review.

import (
	"strings"
	"testing"
)

// A verdict ends a review. A second review of the same pull request through the same
// Set starts with nothing recorded, so it cannot link a finding the first review made
// and the author has since fixed.
func TestASubmittedVerdictEndsTheReviewsRecord(t *testing.T) {
	hub := newFakeHub(t)
	set := newSet(t, hub, nil)
	post := `{"findings":[{"path":"a.go","line":1,"rule":"r","summary":"s","failure":"f"}]}`
	if _, err := invoke(t, toolNamed(t, set, "github_comment"), post); err != nil {
		t.Fatalf("post: %v", err)
	}
	if _, err := invoke(t, toolNamed(t, set, "github_submit_review"), `{"event":"REQUEST_CHANGES","conclusion":"Fix it."}`); err != nil {
		t.Fatalf("first submit: %v", err)
	}

	// A second review, after the author fixed it, finding nothing.
	if _, err := invoke(t, toolNamed(t, set, "github_submit_review"), `{"event":"COMMENT","conclusion":"Nothing blocking."}`); err != nil {
		t.Fatalf("second submit: %v", err)
	}
	body, _ := hub.submittedText.Load().(string)
	if body != "Nothing blocking." {
		t.Fatalf("second verdict = %q, want just the conclusion: the first review's finding was linked again", body)
	}
}

// A failed submission keeps the record, so a retry still links what the review found.
func TestAFailedVerdictKeepsTheFindings(t *testing.T) {
	hub := newFakeHub(t)
	set := newSet(t, hub, nil)
	post := `{"findings":[{"path":"a.go","line":1,"rule":"r","summary":"s","failure":"f"}]}`
	if _, err := invoke(t, toolNamed(t, set, "github_comment"), post); err != nil {
		t.Fatalf("post: %v", err)
	}

	hub.failReviews.Store(true)
	if _, err := invoke(t, toolNamed(t, set, "github_submit_review"), `{"event":"REQUEST_CHANGES","conclusion":"Fix it."}`); err == nil {
		t.Fatal("a failing submission must error")
	}
	hub.failReviews.Store(false)
	if _, err := invoke(t, toolNamed(t, set, "github_submit_review"), `{"event":"REQUEST_CHANGES","conclusion":"Fix it."}`); err != nil {
		t.Fatalf("retry: %v", err)
	}
	body, _ := hub.submittedText.Load().(string)
	if !strings.Contains(body, "One finding:") {
		t.Fatalf("retried verdict = %q, want it to still link the finding", body)
	}
}

// The pull request is bound to the run (Config.Number), never taken from a tool call.
// A number in a tool's input is ignored, so a review whose diff names another pull
// request cannot be steered into commenting or casting a verdict there. The fake hub
// routes only the bound pull request (#7); that these writes succeed is the proof they
// landed on #7 and not on the #999 the input names.
func TestToolInputCannotRedirectToAnotherPullRequest(t *testing.T) {
	hub := newFakeHub(t)
	set := newSet(t, hub, nil) // bound to #7

	comment := `{"number":999,"findings":[{"path":"a.go","line":1,"rule":"r","summary":"s","failure":"f"}]}`
	if _, err := invoke(t, toolNamed(t, set, "github_comment"), comment); err != nil {
		t.Fatalf("comment: %v", err)
	}
	if got := hub.created.Load(); got != 1 {
		t.Fatalf("created = %d, want 1: the comment did not land on the bound pull request", got)
	}

	verdict := `{"number":999,"event":"REQUEST_CHANGES","conclusion":"Fix it."}`
	if _, err := invoke(t, toolNamed(t, set, "github_submit_review"), verdict); err != nil {
		t.Fatalf("submit: %v", err)
	}
	body, _ := hub.submittedText.Load().(string)
	if !strings.Contains(body, "One finding:") {
		t.Fatalf("verdict body = %q, want it to link the finding posted on the bound pull request", body)
	}
}

// A reviewer may re-propose a finding it already posted in the same run: the comment is
// updated, not duplicated, so the verdict must count it once. A per-call list would say
// "2 findings" and link the same line twice.
func TestVerdictCountsARepostedFindingOnce(t *testing.T) {
	hub := newFakeHub(t)
	set := newSet(t, hub, nil)
	post := `{"findings":[{"path":"a.go","line":1,"rule":"r","summary":"s","failure":"f"}]}`

	for range 2 {
		if _, err := invoke(t, toolNamed(t, set, "github_comment"), post); err != nil {
			t.Fatalf("post: %v", err)
		}
	}
	verdict := `{"event":"REQUEST_CHANGES","conclusion":"One thing to fix."}`
	if _, err := invoke(t, toolNamed(t, set, "github_submit_review"), verdict); err != nil {
		t.Fatalf("submit: %v", err)
	}

	body, _ := hub.submittedText.Load().(string)
	if !strings.Contains(body, "One finding:") {
		t.Errorf("verdict body = %q, want one finding counted once", body)
	}
	if n := strings.Count(body, "`a.go:1`"); n != 1 {
		t.Errorf("verdict links a.go:1 %d times, want 1: %q", n, body)
	}
}

// A finding from an earlier round is not this verdict's finding. The author fixed it,
// so the reviewer does not re-propose it, and a verdict that linked it anyway would
// contradict itself: an approval whose body cites an obsolete defect says both that
// nothing blocks the merge and that something does.
func TestVerdictDoesNotLinkStaleFindingsFromEarlierRounds(t *testing.T) {
	hub := newFakeHub(t)
	// A marked comment left over from a previous review round, on a defect since fixed.
	hub.add("review", "<!-- flynn-review:abc123 -->\n<!-- flynn-rule:r -->\nthe old finding")
	set := newSet(t, hub, nil)

	// This round finds nothing and submits its verdict directly.
	verdict := `{"event":"COMMENT","conclusion":"Nothing blocking."}`
	if _, err := invoke(t, toolNamed(t, set, "github_submit_review"), verdict); err != nil {
		t.Fatalf("submit: %v", err)
	}
	body, _ := hub.submittedText.Load().(string)
	if body != "Nothing blocking." {
		t.Fatalf("verdict body = %q, want just the conclusion: a stale finding was linked", body)
	}
}

// The verdict lists only the reviewer's own findings. A maintainer's inline comment
// carries no marker, and a verdict that claimed it as a finding would be citing someone
// else's words as its own.
func TestVerdictListsOnlyTheReviewersOwnComments(t *testing.T) {
	hub := newFakeHub(t)
	hub.add("review", "a human asking a question")
	set := newSet(t, hub, nil)

	post := `{"findings":[{"path":"a.go","line":1,"rule":"r","summary":"s","failure":"f"}]}`
	if _, err := invoke(t, toolNamed(t, set, "github_comment"), post); err != nil {
		t.Fatalf("post: %v", err)
	}
	verdict := `{"event":"REQUEST_CHANGES","conclusion":"One thing to fix."}`
	if _, err := invoke(t, toolNamed(t, set, "github_submit_review"), verdict); err != nil {
		t.Fatalf("submit: %v", err)
	}

	body, _ := hub.submittedText.Load().(string)
	if !strings.HasPrefix(body, "One thing to fix.") {
		t.Errorf("verdict body does not open with the conclusion: %q", body)
	}
	if strings.Contains(body, "human asking") {
		t.Errorf("the verdict claimed a human's comment as its finding: %q", body)
	}
	if n := strings.Count(body, "#discussion_r"); n != 1 {
		t.Errorf("verdict links %d comments, want exactly its own 1: %q", n, body)
	}
	if !strings.Contains(body, "One finding:") {
		t.Errorf("verdict does not list its finding: %q", body)
	}
}

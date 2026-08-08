package github_test

// Idempotency of the inline comments. Re-running a review updates its own prior
// comments rather than duplicating them, which is the single most-filed complaint
// against AI reviewers. Identity is the location and the rule, so rewording a finding
// edits the existing comment while a new line is a new finding. A human's comment
// carries no marker and is never adopted.

import "testing"

// Re-running a review must update its own prior comments, never duplicate them.
// This is the single most-filed complaint against AI reviewers.
func TestCommentReconcilesInsteadOfDuplicating(t *testing.T) {
	hub := newFakeHub(t)
	set := newSet(t, hub, nil)
	tool := toolNamed(t, set, "github_comment")

	const findings = `{"findings":[
      {"path":"a.go","line":12,"rule":"nil-deref","summary":"cfg may be nil","failure":"cfg=nil panics at a.go:12"}]}`

	if _, err := invoke(t, tool, findings); err != nil {
		t.Fatalf("first post: %v", err)
	}
	if got := hub.created.Load(); got != 1 {
		t.Fatalf("created = %d, want 1", got)
	}

	// Same finding, second run: reconciled in place.
	if _, err := invoke(t, tool, findings); err != nil {
		t.Fatalf("second post: %v", err)
	}
	if got := hub.created.Load(); got != 1 {
		t.Fatalf("created = %d after re-run, want still 1 (duplicate posted)", got)
	}
	if got := hub.updated.Load(); got != 1 {
		t.Fatalf("updated = %d after re-run, want 1", got)
	}
}

// Rewording a finding at the same location and rule updates the existing comment
// rather than orphaning it, because identity excludes the body.
func TestCommentIdentityExcludesBody(t *testing.T) {
	hub := newFakeHub(t)
	set := newSet(t, hub, nil)
	tool := toolNamed(t, set, "github_comment")

	first := `{"findings":[{"path":"a.go","line":12,"rule":"nil-deref","summary":"cfg may be nil","failure":"panics"}]}`
	second := `{"findings":[{"path":"a.go","line":12,"rule":"nil-deref","summary":"cfg is nil on the error path","failure":"cfg=nil panics"}]}`

	if _, err := invoke(t, tool, first); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := invoke(t, tool, second); err != nil {
		t.Fatalf("second: %v", err)
	}
	if got := hub.created.Load(); got != 1 {
		t.Fatalf("created = %d, want 1: a reworded finding must update, not duplicate", got)
	}
}

// A finding at a different line is a different finding.
func TestCommentDistinctLinesAreDistinctFindings(t *testing.T) {
	hub := newFakeHub(t)
	set := newSet(t, hub, nil)
	tool := toolNamed(t, set, "github_comment")

	in := `{"findings":[
      {"path":"a.go","line":12,"rule":"r","summary":"s","failure":"f"},
      {"path":"a.go","line":30,"rule":"r","summary":"s","failure":"f"}]}`
	if _, err := invoke(t, tool, in); err != nil {
		t.Fatalf("post: %v", err)
	}
	if got := hub.created.Load(); got != 2 {
		t.Fatalf("created = %d, want 2", got)
	}
}

// A review posts nothing to the pull request's main thread. Findings live on their
// lines, and the verdict carries the conclusion, so a summary comment would be the
// same words a third time. The tool refuses an empty batch rather than treating it as
// a summary-only review.
func TestCommentPostsNothingToTheMainThread(t *testing.T) {
	hub := newFakeHub(t)
	set := newSet(t, hub, nil)
	tool := toolNamed(t, set, "github_comment")

	if _, err := invoke(t, tool, `{"findings":[]}`); err == nil {
		t.Fatal("a review with no findings must not post a comment")
	}
	if n := len(hub.bodies("issue")); n != 0 {
		t.Fatalf("issue comments = %d, want 0: a review never writes to the main thread", n)
	}

	one := `{"findings":[{"path":"a.go","line":3,"rule":"r","summary":"s","failure":"f"}]}`
	if _, err := invoke(t, tool, one); err != nil {
		t.Fatalf("posting a finding: %v", err)
	}
	if n := len(hub.bodies("issue")); n != 0 {
		t.Fatalf("issue comments = %d after posting a finding, want 0", n)
	}
}

// A human's comment carries no marker and must never be adopted or overwritten.
func TestReconcileIgnoresHumanComments(t *testing.T) {
	hub := newFakeHub(t)
	hub.add("review", "I think this is fine actually")
	hub.add("issue", "thanks for the patch!")
	set := newSet(t, hub, nil)
	tool := toolNamed(t, set, "github_comment")

	in := `{"findings":[{"path":"a.go","line":12,"rule":"r","summary":"s","failure":"f"}]}`
	if _, err := invoke(t, tool, in); err != nil {
		t.Fatalf("post: %v", err)
	}
	if got := hub.updated.Load(); got != 0 {
		t.Fatalf("updated = %d, want 0: a human comment was rewritten", got)
	}
	if got := hub.created.Load(); got != 1 {
		t.Fatalf("created = %d, want 1", got)
	}
}

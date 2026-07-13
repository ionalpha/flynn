package github_test

import (
	"strings"
	"testing"

	"github.com/ionalpha/flynn/tools/github"
)

// TestMalformedToolInputIsRefused checks each writing tool refuses input that is not
// the JSON its schema describes, rather than acting on a zero value: an unparsable
// comment call must not post an empty finding, and an unparsable verdict must not
// submit an empty review.
func TestMalformedToolInputIsRefused(t *testing.T) {
	hub := newFakeHub(t)
	set := newSet(t, hub, nil)

	for _, name := range []string{"github_comment", "github_submit_review"} {
		if _, err := invoke(t, toolNamed(t, set, name), `{"findings":`); err == nil {
			t.Errorf("%s accepted malformed input", name)
		}
	}
	if hub.created.Load() != 0 || hub.submittedBody.Load() != nil {
		t.Error("a malformed call must reach GitHub with nothing")
	}
}

// TestAVerdictNeedsAConclusion checks the verdict body is required. GitHub refuses a
// review with no body, so an absent conclusion is caught here where the model can read
// the reason, not as a status code it cannot.
func TestAVerdictNeedsAConclusion(t *testing.T) {
	hub := newFakeHub(t)
	set := newSet(t, hub, nil)

	_, err := invoke(t, toolNamed(t, set, "github_submit_review"), `{"event":"COMMENT","conclusion":"  "}`)
	if err == nil || !strings.Contains(err.Error(), "one-sentence conclusion") {
		t.Fatalf("submit_review error = %v, want a missing-conclusion error", err)
	}
	if hub.submittedBody.Load() != nil {
		t.Error("no review should have been submitted")
	}
}

// TestARuleThatWouldBreakItsOwnTagIsRefused checks a finding whose rule cannot live
// inside the HTML comment it is rendered in is refused. A rule carrying "-->" or a
// newline would close the tag early and detach the finding from its own claim, so it
// never reaches the pull request.
func TestARuleThatWouldBreakItsOwnTagIsRefused(t *testing.T) {
	hub := newFakeHub(t)
	set := newSet(t, hub, nil)

	for _, rule := range []string{"bad --> rule", "bad\nrule"} {
		in := `{"findings":[{"path":"a.go","line":1,"rule":` + quoteJSON(rule) +
			`,"summary":"s","failure":"f"}]}`
		_, err := invoke(t, toolNamed(t, set, "github_comment"), in)
		if err == nil || !strings.Contains(err.Error(), "cannot be embedded") {
			t.Errorf("comment(rule=%q) error = %v, want a refusal", rule, err)
		}
	}
	if hub.created.Load() != 0 {
		t.Error("nothing should have been posted")
	}
}

// quoteJSON renders s as a JSON string literal.
func quoteJSON(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// TestAReviewerThatCannotLearnItsOwnLoginDoesNothing checks the identity failure is
// propagated by every tool that needs it. Without a login the reviewer cannot tell its
// own comments from a maintainer's, so it must fail loudly rather than adopt or
// overwrite somebody else's.
func TestAReviewerThatCannotLearnItsOwnLoginDoesNothing(t *testing.T) {
	hub := newFakeHub(t)
	hub.graphqlError = "Resource not accessible by integration"
	set := newSet(t, hub, func(cfg *github.Config) {
		cfg.SelfLogin = "" // force the login to be resolved from the credential
		cfg.AllowApprove = true
	})

	if _, err := invoke(t, toolNamed(t, set, "github_pr_fetch"), `{}`); err == nil ||
		!strings.Contains(err.Error(), "reviewer's own login") {
		t.Errorf("pr_fetch error = %v, want the identity failure", err)
	}

	in := `{"findings":[{"path":"a.go","line":1,"rule":"r","summary":"s","failure":"f"}]}`
	if _, err := invoke(t, toolNamed(t, set, "github_comment"), in); err == nil ||
		!strings.Contains(err.Error(), "reviewer's own login") {
		t.Errorf("comment error = %v, want the identity failure", err)
	}
	if hub.created.Load() != 0 {
		t.Error("no comment should have been posted without an identity")
	}

	if _, err := invoke(t, toolNamed(t, set, "github_submit_review"),
		`{"event":"APPROVE","conclusion":"looks good"}`); err == nil ||
		!strings.Contains(err.Error(), "reviewer's own login") {
		t.Errorf("submit_review error = %v, want the identity failure", err)
	}
	if hub.submittedBody.Load() != nil {
		t.Error("no verdict should have been submitted without an identity")
	}
}

// TestAFailedFetchOfThePullRequestStopsEveryTool checks the pull request's own
// endpoint failing is reported by each tool rather than worked around: a comment
// anchored to an unknown head, or a verdict on an unknown author, would be worse than
// no answer.
func TestAFailedFetchOfThePullRequestStopsEveryTool(t *testing.T) {
	hub := newFakeHub(t)
	hub.failPR.Store(true)
	set := newSet(t, hub, nil)

	if _, err := invoke(t, toolNamed(t, set, "github_pr_fetch"), `{}`); err == nil {
		t.Error("pr_fetch must fail when the pull request cannot be read")
	}
	in := `{"findings":[{"path":"a.go","line":1,"rule":"r","summary":"s","failure":"f"}]}`
	if _, err := invoke(t, toolNamed(t, set, "github_comment"), in); err == nil {
		t.Error("comment must fail when the pull request cannot be read")
	}
	if _, err := invoke(t, toolNamed(t, set, "github_submit_review"),
		`{"event":"COMMENT","conclusion":"c"}`); err == nil {
		t.Error("submit_review must fail when the pull request cannot be read")
	}
	if hub.created.Load() != 0 || hub.submittedBody.Load() != nil {
		t.Error("nothing should have been written")
	}
}

// TestAFailedCommentWriteIsReported checks a create and an update that GitHub refuses
// are both surfaced, so a reviewer never reports a finding as posted when it is not.
func TestAFailedCommentWriteIsReported(t *testing.T) {
	in := `{"findings":[{"path":"a.go","line":1,"rule":"r","summary":"s","failure":"f"}]}`

	t.Run("create", func(t *testing.T) {
		hub := newFakeHub(t)
		hub.failCommentWrite.Store(true)
		set := newSet(t, hub, nil)
		if _, err := invoke(t, toolNamed(t, set, "github_comment"), in); err == nil ||
			!strings.Contains(err.Error(), "500") {
			t.Fatalf("comment error = %v, want the write failure", err)
		}
	})

	t.Run("update", func(t *testing.T) {
		hub := newFakeHub(t)
		set := newSet(t, hub, nil)
		// Post it once so the second call reconciles onto the existing comment, then make
		// the write fail.
		if _, err := invoke(t, toolNamed(t, set, "github_comment"), in); err != nil {
			t.Fatalf("comment: %v", err)
		}
		hub.failCommentWrite.Store(true)
		if _, err := invoke(t, toolNamed(t, set, "github_comment"), in); err == nil ||
			!strings.Contains(err.Error(), "500") {
			t.Fatalf("comment error = %v, want the update failure", err)
		}
		if hub.updated.Load() != 0 {
			t.Error("a refused update must not be counted as applied")
		}
	})
}

// TestAFailedListingOfExistingCommentsStopsTheReview checks the reviewer refuses to
// act when it cannot read what it already posted. Reconciling against a listing that
// failed would open a second conversation about a finding already on the diff.
func TestAFailedListingOfExistingCommentsStopsTheReview(t *testing.T) {
	hub := newFakeHub(t)
	hub.failCommentList.Store(true)
	set := newSet(t, hub, nil)

	if _, err := invoke(t, toolNamed(t, set, "github_pr_fetch"), `{}`); err == nil {
		t.Error("pr_fetch must fail when the existing comments cannot be listed")
	}
	in := `{"findings":[{"path":"a.go","line":1,"rule":"r","summary":"s","failure":"f"}]}`
	if _, err := invoke(t, toolNamed(t, set, "github_comment"), in); err == nil {
		t.Error("comment must fail when the existing comments cannot be listed")
	}
	if hub.created.Load() != 0 {
		t.Error("nothing should have been posted")
	}
}

// TestPostedFindingsAreOrderedByPathThenLine checks the findings handed back to the
// reviewer are in diff order, so a pull request nothing has happened to reads the same
// way twice.
func TestPostedFindingsAreOrderedByPathThenLine(t *testing.T) {
	hub := newFakeHub(t)
	set := newSet(t, hub, nil)

	in := `{"findings":[
      {"path":"z.go","line":2,"rule":"r1","summary":"s","failure":"f"},
      {"path":"a.go","line":9,"rule":"r2","summary":"s","failure":"f"},
      {"path":"a.go","line":3,"rule":"r3","summary":"s","failure":"f"}
    ]}`
	if _, err := invoke(t, toolNamed(t, set, "github_comment"), in); err != nil {
		t.Fatalf("comment: %v", err)
	}

	out, err := invoke(t, toolNamed(t, set, "github_pr_fetch"), `{}`)
	if err != nil {
		t.Fatalf("pr_fetch: %v", err)
	}
	want := []string{`"path":"a.go","line":3`, `"path":"a.go","line":9`, `"path":"z.go","line":2`}
	at := make([]int, len(want))
	for i, w := range want {
		at[i] = strings.Index(out, w)
		if at[i] < 0 {
			t.Fatalf("posted findings do not carry %s:\n%s", w, out)
		}
	}
	for i := 1; i < len(at); i++ {
		if at[i-1] > at[i] {
			t.Fatalf("posted findings are out of diff order:\n%s", out)
		}
	}
}

package github_test

// Review threads: reading them, and closing the reviewer's own stale ones. Resolution
// is scoped and conditional. Only threads this reviewer opened are closed, never when
// the diff was truncated, and a failed resolution does not fail the verdict. A reviewer
// without permission to resolve minimizes instead. Listing refuses to stop at its
// pagination cap rather than silently reporting a partial set as the whole.

import (
	"strings"
	"testing"

	"github.com/ionalpha/flynn/tools/github"
)

// A stale finding is a thread the reviewer no longer stands behind. It closes its own,
// leaves a human's alone, and leaves open the one it just raised again.
func TestVerdictResolvesItsOwnStaleThreadsOnly(t *testing.T) {
	hub := newFakeHub(t)
	set := newSet(t, hub, func(c *github.Config) { c.SelfLogin = "vouchbot" })

	// Post the finding first, so the thread below carries the marker the tool really
	// writes rather than one the test guessed.
	post := `{"findings":[{"path":"a.go","line":1,"rule":"r","summary":"s","failure":"f"}]}`
	if _, err := invoke(t, toolNamed(t, set, "github_comment"), post); err != nil {
		t.Fatalf("post: %v", err)
	}
	bodies := hub.bodies("review")
	if len(bodies) != 1 {
		t.Fatalf("want 1 posted comment, got %d", len(bodies))
	}

	// The thread for the finding this review just raised again.
	hub.addThread(fakeThread{id: "T-open", canResolve: true, comments: []fakeThreadComment{{body: bodies[0], author: "vouchbot"}}})
	// A finding from an earlier round, fixed since: a different marker, not re-raised.
	hub.addThread(fakeThread{id: "T-stale", outdated: true, canResolve: true, comments: []fakeThreadComment{
		{body: "<!-- flynn-review:deadbe -->\n<!-- flynn-rule:r -->\nthe old finding", author: "vouchbot"},
	}})
	// A maintainer's own thread. Never ours to close.
	hub.addThread(fakeThread{id: "T-human", outdated: true, canResolve: true, comments: []fakeThreadComment{
		{body: "why is this here?", author: "a-maintainer"},
	}})
	// Ours, but a person replied in it. Somebody is talking.
	hub.addThread(fakeThread{id: "T-reply", outdated: true, canResolve: true, comments: []fakeThreadComment{
		{body: "<!-- flynn-review:c0ffee -->\n<!-- flynn-rule:r -->\na finding", author: "vouchbot"},
		{body: "disagree, this is intentional", author: "a-maintainer"},
	}})

	out, err := invoke(t, toolNamed(t, set, "github_submit_review"), `{"event":"REQUEST_CHANGES","conclusion":"Still one thing."}`)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	got := hub.resolvedThreads()
	if len(got) != 1 || got[0] != "T-stale" {
		t.Fatalf("resolved %v, want exactly [T-stale]", got)
	}
	if !strings.Contains(out, "resolved 1 stale thread") {
		t.Errorf("tool result does not report the resolution: %q", out)
	}
}

// A truncated diff means the reviewer never saw the whole change. A finding it did not
// raise may be a defect it did not read, so nothing is resolved. Same invariant that
// refuses an approval on a partial diff.
func TestNoThreadIsResolvedWhenTheDiffWasTruncated(t *testing.T) {
	hub := newFakeHub(t)
	hub.changedFiles = 4000 // GitHub's own count, past the fetch cap
	hub.addThread(fakeThread{id: "T-stale", outdated: true, canResolve: true, comments: []fakeThreadComment{
		{body: "<!-- flynn-review:deadbe -->\n<!-- flynn-rule:r -->\nthe old finding", author: "vouchbot"},
	}})
	set := newSet(t, hub, nil)

	if _, err := invoke(t, toolNamed(t, set, "github_submit_review"), `{"event":"COMMENT","conclusion":"Partial read."}`); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if got := hub.resolvedThreads(); len(got) != 0 {
		t.Fatalf("resolved %v on a truncated diff, want none", got)
	}
}

// The verdict is the review's output; resolving is housekeeping after it. A GraphQL
// failure must not cost a submitted verdict its success, and must not be silent: an
// unresolved thread blocks the merge wherever thread resolution is required.
func TestAFailedResolutionDoesNotFailTheVerdict(t *testing.T) {
	hub := newFakeHub(t)
	hub.graphqlError = "Resource not accessible by integration"
	set := newSet(t, hub, func(c *github.Config) { c.SelfLogin = "vouchbot" })

	out, err := invoke(t, toolNamed(t, set, "github_submit_review"), `{"event":"COMMENT","conclusion":"Looks fine."}`)
	if err != nil {
		t.Fatalf("a failed resolution must not fail the verdict: %v", err)
	}
	if got, _ := hub.submittedBody.Load().(string); got != "COMMENT" {
		t.Fatalf("verdict submitted = %q, want COMMENT", got)
	}
	if !strings.Contains(out, "could not resolve") || !strings.Contains(out, "not accessible") {
		t.Errorf("the failure was swallowed: %q", out)
	}
}

// A blocking verdict must survive a transient failure of the changed-files endpoint.
// Diff coverage gates an approval and gates thread resolution, which is housekeeping
// that runs after the verdict. A reviewer that found a real defect must still be able
// to say so when GitHub hiccups on a request the verdict does not depend on.
func TestATransientFilesFailureDoesNotBlockABlockingVerdict(t *testing.T) {
	hub := newFakeHub(t)
	hub.addThread(fakeThread{id: "T-stale", outdated: true, canResolve: true, comments: []fakeThreadComment{
		{body: "<!-- flynn-review:deadbe -->\n<!-- flynn-rule:r -->\nan old finding", author: "vouchbot"},
	}})
	set := newSet(t, hub, func(c *github.Config) { c.SelfLogin = "vouchbot" })
	hub.failFiles.Store(true)

	out, err := invoke(t, toolNamed(t, set, "github_submit_review"), `{"event":"REQUEST_CHANGES","conclusion":"A real defect."}`)
	if err != nil {
		t.Fatalf("a transient files failure blocked the verdict: %v", err)
	}
	// Resolution was skipped, and the reviewer must say so: a repository whose merge is
	// blocked by its stale conversations would otherwise see it choose to leave them.
	if !strings.Contains(out, "the diff could not be read") {
		t.Errorf("the skipped resolution was not reported: %q", out)
	}
	if got, _ := hub.submittedBody.Load().(string); got != "REQUEST_CHANGES" {
		t.Fatalf("verdict submitted = %q, want REQUEST_CHANGES", got)
	}
	// The diff could not be read, so its completeness is unknown and nothing is closed.
	if got := hub.resolvedThreads(); len(got) != 0 {
		t.Errorf("resolved %v on a diff that could not be read", got)
	}
}

// An approval still depends on reading the diff: it asserts the whole change was
// reviewed, so a coverage read that fails refuses the approval rather than guessing.
func TestATransientFilesFailureRefusesAnApproval(t *testing.T) {
	hub := newFakeHub(t)
	set := newSet(t, hub, func(c *github.Config) { c.AllowApprove = true; c.SelfLogin = "vouchbot" })
	hub.failFiles.Store(true)

	if _, err := invoke(t, toolNamed(t, set, "github_submit_review"), `{"event":"APPROVE","conclusion":"Looks fine."}`); err == nil {
		t.Fatal("an approval must not be submitted when the diff could not be read")
	}
	if got, _ := hub.submittedBody.Load().(string); got == "APPROVE" {
		t.Fatal("an approval reached the API on an unread diff")
	}
}

// A pull request with more conversations than one page must still have all of them
// read. A reviewer that stopped at the first page would leave every later thread open
// forever, blocking the merge wherever thread resolution is required.
func TestReviewThreadsFollowPagination(t *testing.T) {
	hub := newFakeHub(t)
	for _, id := range []string{"T1", "T2", "T3"} {
		hub.addThread(fakeThread{id: id, outdated: true, canResolve: true, comments: []fakeThreadComment{
			{body: "<!-- flynn-review:" + id + " -->\n<!-- flynn-rule:r -->\nan old finding", author: "vouchbot"},
		}})
	}
	set := newSet(t, hub, func(c *github.Config) { c.SelfLogin = "vouchbot[bot]" })

	if _, err := invoke(t, toolNamed(t, set, "github_submit_review"), `{"event":"COMMENT","conclusion":"Nothing blocking."}`); err != nil {
		t.Fatalf("submit: %v", err)
	}
	got := hub.resolvedThreads()
	if len(got) != 3 {
		t.Fatalf("resolved %v, want all three: the reviewer stopped at the first page", got)
	}
}

// Resolving a conversation needs write access to the repository. A reviewer holding
// only pull_requests:write cannot resolve, and must not pretend it did. It folds its
// own stale comment away, marked outdated, and reports that the thread is still open.
//
// This is not hypothetical: GitHub reports viewerCanResolve=false for an App installed
// with contents:read, which is exactly the permission set that lets a reviewer review
// without ever being able to push.
func TestAReviewerThatMayNotResolveMinimizesInstead(t *testing.T) {
	hub := newFakeHub(t)
	hub.addThread(fakeThread{id: "T-stale", outdated: true, canResolve: false, comments: []fakeThreadComment{
		{body: "<!-- flynn-review:deadbe -->\n<!-- flynn-rule:r -->\nan old finding", author: "vouchbot"},
	}})
	set := newSet(t, hub, func(c *github.Config) { c.SelfLogin = "vouchbot[bot]" })

	out, err := invoke(t, toolNamed(t, set, "github_submit_review"), `{"event":"COMMENT","conclusion":"Nothing blocking."}`)
	if err != nil {
		t.Fatalf("a reviewer that may not resolve must still submit its verdict: %v", err)
	}
	if got := hub.resolvedThreads(); len(got) != 0 {
		t.Errorf("resolved %v without permission to resolve", got)
	}
	if got := hub.minimizedComments(); len(got) != 1 || got[0] != "T-stale-c0" {
		t.Fatalf("minimized %v, want the stale finding's own comment", got)
	}
	if !strings.Contains(out, "folded away but left unresolved") || !strings.Contains(out, "write access") {
		t.Errorf("the reviewer did not say the thread is still open: %q", out)
	}
}

// A reviewer that may resolve resolves, and minimizes nothing: folding a comment away
// on top of closing its thread would be noise.
func TestAReviewerThatMayResolveDoesNotMinimize(t *testing.T) {
	hub := newFakeHub(t)
	hub.addThread(fakeThread{id: "T-stale", outdated: true, canResolve: true, comments: []fakeThreadComment{
		{body: "<!-- flynn-review:deadbe -->\n<!-- flynn-rule:r -->\nan old finding", author: "vouchbot"},
	}})
	set := newSet(t, hub, func(c *github.Config) { c.SelfLogin = "vouchbot[bot]" })

	if _, err := invoke(t, toolNamed(t, set, "github_submit_review"), `{"event":"COMMENT","conclusion":"Nothing blocking."}`); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if got := hub.resolvedThreads(); len(got) != 1 || got[0] != "T-stale" {
		t.Fatalf("resolved %v, want [T-stale]", got)
	}
	if got := hub.minimizedComments(); len(got) != 0 {
		t.Errorf("minimized %v on top of resolving", got)
	}
}

// A pull request with more conversations than the reviewer will page through is not a
// pull request it has tidied up. Returning the pages it did read would report a
// complete pass, and every thread past the cap would stay open while the reviewer said
// otherwise. A partial read is an error.
func TestReadingThreadsRefusesToStopAtTheCap(t *testing.T) {
	hub := newFakeHub(t)
	hub.endlessThreads = true
	set := newSet(t, hub, func(c *github.Config) { c.SelfLogin = "vouchbot[bot]" })

	out, err := invoke(t, toolNamed(t, set, "github_submit_review"), `{"event":"COMMENT","conclusion":"Nothing blocking."}`)
	if err != nil {
		t.Fatalf("the verdict must still land: %v", err)
	}
	if !strings.Contains(out, "could not resolve") || !strings.Contains(out, "pages of review threads") {
		t.Errorf("a truncated read of the conversations was reported as a clean pass: %q", out)
	}
	if got := hub.resolvedThreads(); len(got) != 0 {
		t.Errorf("resolved %v from a partial read of the conversations", got)
	}
}

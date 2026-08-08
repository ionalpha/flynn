package github_test

// Whose comment is whose. The reviewer identifies its own comments by author, learned
// live from the credential, and not by the marker text alone, which anyone can copy. A
// maintainer's comment is never adopted or overwritten, and a comment whose anchor has
// left the diff is not offered back as a posted finding.

import (
	"strings"
	"testing"

	"github.com/ionalpha/flynn/tools/github"
)

// A maintainer who quotes a finding back at the reviewer has copied the marker into
// their own comment. The reviewer must not read that as licence to close their thread.
func TestAThreadIsNotOursBecauseItCarriesOurMarker(t *testing.T) {
	hub := newFakeHub(t)
	hub.addThread(fakeThread{id: "T-copied", outdated: true, canResolve: true, comments: []fakeThreadComment{
		{body: "<!-- flynn-review:deadbe -->\nquoting your finding back at you", author: "a-maintainer"},
	}})
	set := newSet(t, hub, func(c *github.Config) { c.SelfLogin = "vouchbot" })

	if _, err := invoke(t, toolNamed(t, set, "github_submit_review"), `{"event":"COMMENT","conclusion":"Nothing blocking."}`); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if got := hub.resolvedThreads(); len(got) != 0 {
		t.Fatalf("resolved %v: a copied marker is not authorship", got)
	}
}

// A maintainer's comment carrying a copied marker is neither adopted as a finding nor
// rewritten in place. The marker says which finding a comment stands for; it says
// nothing about who wrote it, and it is plain text anyone can copy out of a body.
func TestAMaintainersCommentIsNeverAdoptedOrOverwritten(t *testing.T) {
	hub := newFakeHub(t)
	hub.commentAuthor = "a-maintainer" // everything REST reports was written by a person
	set := newSet(t, hub, nil)

	// Seed a comment carrying the exact marker the finding below produces.
	f := `{"findings":[{"path":"a.go","line":1,"rule":"r","summary":"s","failure":"f"}]}`
	if _, err := invoke(t, toolNamed(t, set, "github_comment"), f); err != nil {
		t.Fatalf("post: %v", err)
	}
	// The reviewer's own comment is stored, but REST attributes it to the maintainer, so
	// on the next pass it must not be treated as the reviewer's.
	if _, err := invoke(t, toolNamed(t, set, "github_comment"), f); err != nil {
		t.Fatalf("re-post: %v", err)
	}
	if got := hub.updated.Load(); got != 0 {
		t.Errorf("the reviewer rewrote a comment it did not write (%d update(s))", got)
	}

	out, err := invoke(t, toolNamed(t, set, "github_pr_fetch"), `{}`)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if strings.Contains(out, "posted_findings") {
		t.Errorf("the reviewer claimed a maintainer's comment as its own finding: %s", out)
	}
}

// The reviewer learns its own login from the credential when none is configured. REST's
// /user refuses an installation token, so viewer{login} is the one question both a
// personal token and an App installation answer.
func TestTheReviewerLearnsItsOwnLoginFromTheCredential(t *testing.T) {
	hub := newFakeHub(t)
	hub.viewerLogin = "vouchbot[bot]"
	hub.commentAuthor = "vouchbot" // GraphQL drops the suffix; REST keeps it
	set := newSet(t, hub, func(c *github.Config) { c.SelfLogin = "" })

	f := `{"findings":[{"path":"a.go","line":1,"rule":"r","summary":"s","failure":"f"}]}`
	if _, err := invoke(t, toolNamed(t, set, "github_comment"), f); err != nil {
		t.Fatalf("post: %v", err)
	}
	if _, err := invoke(t, toolNamed(t, set, "github_comment"), f); err != nil {
		t.Fatalf("re-post: %v", err)
	}
	if got := hub.created.Load(); got != 1 {
		t.Errorf("created = %d, want 1: the reviewer did not recognise its own comment", got)
	}
	if got := hub.updated.Load(); got != 1 {
		t.Errorf("updated = %d, want 1", got)
	}
}

// Running out of pages while listing inline comments is an error, not an answer. The
// list tells the reviewer which of its findings are already posted; a short list is a
// finding it cannot restate, and a finding not restated is a finding retracted.
func TestReviewCommentsRefuseToStopAtTheCap(t *testing.T) {
	hub := newFakeHub(t)
	hub.endlessComments = true
	set := newSet(t, hub, nil)

	if _, err := invoke(t, toolNamed(t, set, "github_pr_fetch"), `{}`); err == nil {
		t.Fatal("a truncated listing of the reviewer's own comments must fail the fetch")
	}
}

// GitHub reports line as null for a comment whose anchor no longer exists in the diff,
// which decodes to zero. Offering it back as a posted finding would have the reviewer
// repost a finding with no line, which the validator refuses, so it could never restate
// it. The comment is omitted; its thread is retracted as outdated instead.
func TestAnOutdatedCommentIsNotOfferedBackAsAPostedFinding(t *testing.T) {
	hub := newFakeHub(t)
	hub.commentLine = 0 // GitHub's null, decoded
	set := newSet(t, hub, nil)

	f := `{"findings":[{"path":"a.go","line":1,"rule":"r","summary":"s","failure":"f"}]}`
	if _, err := invoke(t, toolNamed(t, set, "github_comment"), f); err != nil {
		t.Fatalf("post: %v", err)
	}
	out, err := invoke(t, toolNamed(t, set, "github_pr_fetch"), `{}`)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if strings.Contains(out, "posted_findings") {
		t.Errorf("a finding with no line was offered back, and cannot be reposted: %s", out)
	}
}

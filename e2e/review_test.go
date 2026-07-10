package e2e

import (
	"fmt"
	"strings"
	"testing"
)

// The review e2e scenarios drive the built binary through a whole pull-request review
// against a scripted GitHub and a scripted model. Nothing here reaches the network:
// --api-base points the review at a loopback server, and the model answers from a
// queue, so every assertion is about what the binary actually sent to the API.
//
// The suite exists because the review's guarantees are refusals, and a refusal is only
// worth as much as the evidence that the write never happened. Asserting on the
// command's exit code alone would pass just as well for a reviewer that approved the
// pull request and then printed an error.

const (
	reviewPRRef  = "acme/widgets#7"
	reviewerName = "vouchbot[bot]"
)

// reviewInstance wires an instance to a scripted model and gives it a credential to
// authenticate with, which the fake server requires on every request.
func reviewInstance(t *testing.T, model *fakeOpenAI) *instance {
	t.Helper()
	in := newInstance(t).withModel(model)
	in.setEnv("GITHUB_TOKEN", "ghs-e2e-not-a-real-token")
	return in
}

func reviewArgs(gh *fakeGitHub, extra ...string) []string {
	return append([]string{"review", reviewPRRef, "--api-base", gh.baseURL()}, extra...)
}

// fetchCall is the model's first turn: fetch the pull request.
func fetchCall() oaiReply { return toolCall("call-1", "github_pr_fetch", `{"number":7}`) }

// commentCall posts one inline finding. There is no summary: a finding lives on its
// line, and the verdict links it.
func commentCall(rule string) oaiReply {
	args := fmt.Sprintf(`{"number":7,"findings":[{"path":"limiter.go","line":4,`+
		`"rule":%q,"summary":"Allow always returns true, so no client is ever limited.",`+
		`"failure":"Allow(\"c\") returns true for every call, so a client sending 10k requests per second is never throttled."}]}`, rule)
	return toolCall("call-2", "github_comment", args)
}

// verdictCall submits the given verdict.
func verdictCall(id, event string) oaiReply {
	return toolCall(id, "github_submit_review", fmt.Sprintf(`{"number":7,"event":%q,"conclusion":"Reviewed."}`, event))
}

// TestReviewPostsFindingsAndRequestsChanges is the whole happy path for a blocking
// review: the binary fetches the pull request, posts an inline finding anchored to a
// file and line, maintains one summary comment, submits REQUEST_CHANGES, and exits 3
// so a CI step can gate the merge on it.
func TestReviewPostsFindingsAndRequestsChanges(t *testing.T) {
	gh := newFakeGitHub(t, defaultPR())
	model := newFakeOpenAIQueue(
		t,
		fetchCall(),
		commentCall("always-allows"),
		verdictCall("call-3", "REQUEST_CHANGES"),
		finalText("Requested changes."),
	)
	in := reviewInstance(t, model)

	res := in.run(reviewArgs(gh)...)
	if res.code != 3 {
		t.Fatalf("want exit 3 (changes requested), got %d\n%s", res.code, res.combined())
	}
	if got := gh.verdicts(); len(got) != 1 || got[0] != "REQUEST_CHANGES" {
		t.Fatalf("verdicts submitted to the API = %v, want [REQUEST_CHANGES]", got)
	}

	inline := gh.inlineComments()
	if len(inline) != 1 {
		t.Fatalf("want 1 inline comment, got %d: %+v", len(inline), inline)
	}
	if inline[0].Path != "limiter.go" || inline[0].Line != 4 {
		t.Errorf("inline comment anchored at %s:%d, want limiter.go:4", inline[0].Path, inline[0].Line)
	}
	if !strings.Contains(inline[0].Body, "never throttled") {
		t.Errorf("inline comment body lost the failure scenario: %q", inline[0].Body)
	}
	if got := gh.summaries(); len(got) != 0 {
		t.Fatalf("a review wrote %d comment(s) to the main thread, want 0: %v", len(got), got)
	}

	// The verdict carries the conclusion and a link to the finding, and says it once.
	body := gh.verdictBodies()[0]
	if !strings.HasPrefix(body, "Reviewed.") {
		t.Errorf("verdict body does not open with the conclusion: %q", body)
	}
	if !strings.Contains(body, "One finding:") || !strings.Contains(body, "limiter.go:4") {
		t.Errorf("verdict body does not link the finding: %q", body)
	}
	if strings.Contains(body, "never throttled") {
		t.Errorf("verdict body restates the finding instead of linking it: %q", body)
	}
}

// TestReviewAdvertisesOnlyTheReviewToolsToTheModel pins the reviewer's authority at the
// point it becomes reachable: the tools the binary put in front of the model. A tool
// absent here cannot be called however the model is prompted, and a tool that appeared
// here would be a widening no capability test in-process would catch, because the
// toolset the command assembles is the one that reaches the wire.
func TestReviewAdvertisesOnlyTheReviewToolsToTheModel(t *testing.T) {
	gh := newFakeGitHub(t, defaultPR())
	model := newFakeOpenAIQueue(
		t,
		fetchCall(),
		verdictCall("call-2", "COMMENT"),
		finalText("Nothing blocking."),
	)
	in := reviewInstance(t, model)

	if res := in.run(reviewArgs(gh)...); res.code != 0 {
		t.Fatalf("review failed: exit %d\n%s", res.code, res.combined())
	}

	want := []string{"github_comment", "github_pr_fetch", "github_submit_review"}
	got := append([]string(nil), model.request(t, 0).Tools...)
	if len(got) != len(want) {
		t.Fatalf("the reviewer advertised %v, want exactly %v", got, want)
	}
	seen := map[string]bool{}
	for _, name := range got {
		seen[name] = true
	}
	for _, name := range want {
		if !seen[name] {
			t.Errorf("the reviewer did not advertise %s; advertised %v", name, got)
		}
	}
	if sys := model.request(t, 0).System; !strings.Contains(sys, "reviewing a pull request") {
		t.Errorf("the run did not carry the reviewer's standing instruction; system prompt was %q", sys)
	}
}

// TestReviewApproveIsRefusedWithoutTheFlag proves approval is off by default at the
// only layer that matters: no APPROVE reaches the API. Installing the reviewer must
// not silently add a merge gate to a repository.
func TestReviewApproveIsRefusedWithoutTheFlag(t *testing.T) {
	gh := newFakeGitHub(t, defaultPR())
	model := newFakeOpenAIQueue(
		t,
		fetchCall(),
		verdictCall("call-2", "APPROVE"), // refused at the tool
		verdictCall("call-3", "COMMENT"), // the model settles for a comment
		finalText("Left a comment."),
	)
	in := reviewInstance(t, model)

	res := in.run(reviewArgs(gh)...)
	if res.code != 0 {
		t.Fatalf("want exit 0, got %d\n%s", res.code, res.combined())
	}
	if got := gh.verdicts(); len(got) != 1 || got[0] != "COMMENT" {
		t.Fatalf("verdicts that reached the API = %v, want [COMMENT]: the refused APPROVE must never be submitted", got)
	}
}

// TestReviewApprovesWhenEnabled is the other half: with approval enabled and a
// reviewer identity distinct from the author, a clean pull request receives a formal
// APPROVE, and the command exits 0.
func TestReviewApprovesWhenEnabled(t *testing.T) {
	gh := newFakeGitHub(t, defaultPR())
	model := newFakeOpenAIQueue(
		t,
		fetchCall(),
		verdictCall("call-2", "APPROVE"),
		finalText("Approved."),
	)
	in := reviewInstance(t, model)

	res := in.run(reviewArgs(gh, "--approve", "--as", reviewerName)...)
	if res.code != 0 {
		t.Fatalf("want exit 0, got %d\n%s", res.code, res.combined())
	}
	if got := gh.verdicts(); len(got) != 1 || got[0] != "APPROVE" {
		t.Fatalf("verdicts = %v, want [APPROVE]", got)
	}
	if !strings.Contains(res.combined(), "approve") {
		t.Errorf("the command did not report the approval:\n%s", res.combined())
	}
}

// TestReviewRefusesToApproveItsOwnPullRequest checks the refusal that no configuration
// may switch off. The reviewer is enabled to approve and is the author, which is the
// exact shape of a bot approving the change it opened.
func TestReviewRefusesToApproveItsOwnPullRequest(t *testing.T) {
	pr := defaultPR()
	pr.AuthorLogin = reviewerName
	gh := newFakeGitHub(t, pr)
	model := newFakeOpenAIQueue(
		t,
		fetchCall(),
		verdictCall("call-2", "APPROVE"),
		finalText("Done."),
	)
	in := reviewInstance(t, model)

	res := in.run(reviewArgs(gh, "--approve", "--as", reviewerName)...)
	if res.code == 0 {
		t.Fatalf("a run that submitted no verdict must not exit 0:\n%s", res.combined())
	}
	if got := gh.verdicts(); len(got) != 0 {
		t.Fatalf("a self-approval reached the API: %v", got)
	}
	if gh.sawWrite("/reviews") {
		t.Error("the binary posted to the reviews endpoint despite the refusal")
	}
}

// TestReviewRefusesToApproveATruncatedDiff pins the claim an approval makes. GitHub
// reports more changed files than the fetch cap allows, so the reviewer never saw the
// whole change, and saying "I reviewed this and it is fine" would be a false statement.
// A blocking verdict stays available on the same partial evidence.
func TestReviewRefusesToApproveATruncatedDiff(t *testing.T) {
	pr := defaultPR()
	pr.ChangedFiles = 4000 // GitHub's own count, far past the fetch cap
	gh := newFakeGitHub(t, pr)
	model := newFakeOpenAIQueue(
		t,
		fetchCall(),
		verdictCall("call-2", "APPROVE"),
		verdictCall("call-3", "REQUEST_CHANGES"),
		finalText("Could not read the whole diff."),
	)
	in := reviewInstance(t, model)

	res := in.run(reviewArgs(gh, "--approve", "--as", reviewerName)...)
	if res.code != 3 {
		t.Fatalf("want exit 3 (changes requested), got %d\n%s", res.code, res.combined())
	}
	if got := gh.verdicts(); len(got) != 1 || got[0] != "REQUEST_CHANGES" {
		t.Fatalf("verdicts = %v, want [REQUEST_CHANGES]: an approval on a truncated diff must not reach the API", got)
	}
}

// TestReviewReconcilesRatherThanDuplicating reviews the same pull request twice with
// the same finding. The second run must find its own comment through the fetch and
// update it in place. A reviewer that re-posts on every run buries the pull request it
// is meant to help.
func TestReviewReconcilesRatherThanDuplicating(t *testing.T) {
	gh := newFakeGitHub(t, defaultPR())

	for round := range 2 {
		model := newFakeOpenAIQueue(
			t,
			fetchCall(),
			commentCall("always-allows"),
			verdictCall("call-3", "REQUEST_CHANGES"),
			finalText("Requested changes."),
		)
		in := reviewInstance(t, model)
		if res := in.run(reviewArgs(gh)...); res.code != 3 {
			t.Fatalf("round %d: want exit 3, got %d\n%s", round, res.code, res.combined())
		}
	}

	if got := gh.inlineComments(); len(got) != 1 {
		t.Fatalf("two reviews of the same finding left %d inline comments, want 1: %+v", len(got), got)
	}
	if got := gh.summaries(); len(got) != 0 {
		t.Fatalf("two reviews wrote %d comment(s) to the main thread, want 0: %v", len(got), got)
	}
}

// TestReviewWithoutCredentialsRefusesBeforeCallingTheAPI checks the command fails on a
// missing credential rather than issuing an unauthenticated request, which GitHub would
// answer for public data and the reviewer would then act on.
func TestReviewWithoutCredentialsRefusesBeforeCallingTheAPI(t *testing.T) {
	gh := newFakeGitHub(t, defaultPR())
	model := newFakeOpenAIQueue(t, finalText("unreachable"))
	in := newInstance(t).withModel(model) // no GITHUB_TOKEN

	res := in.run(reviewArgs(gh)...)
	if res.code == 0 {
		t.Fatalf("a review with no credential must not succeed:\n%s", res.combined())
	}
	if !strings.Contains(res.combined(), "no GitHub credentials") {
		t.Errorf("want a credential error, got:\n%s", res.combined())
	}
	if model.count() != 0 {
		t.Errorf("the binary called the model %d time(s) before discovering it had no credential", model.count())
	}
}

// TestReviewSurfacesAnAPIFailure checks that a failing GitHub write fails the review
// rather than being reported as a clean pass. The submit is answered with a 500, so the
// verdict tracker must not record a verdict it never landed.
func TestReviewSurfacesAnAPIFailure(t *testing.T) {
	gh := newFakeGitHub(t, defaultPR())
	gh.failOn("POST /repos/acme/widgets/pulls/7/reviews", 500)
	model := newFakeOpenAIQueue(
		t,
		fetchCall(),
		verdictCall("call-2", "COMMENT"),
		finalText("Left a comment."),
	)
	in := reviewInstance(t, model)

	res := in.run(reviewArgs(gh)...)
	if res.code == 0 {
		t.Fatalf("a review whose verdict never landed must not exit 0:\n%s", res.combined())
	}
}

// TestReviewResolvesItsOwnStaleThreads drives the built binary through the resolve
// path end to end. A finding from an earlier round is no longer raised, so its
// conversation is closed; a maintainer's own conversation is not.
//
// This matters beyond tidiness: a repository that requires review-thread resolution
// cannot merge while the reviewer's stale conversations stay open, so a reviewer that
// never closes them wedges every pull request it touches.
func TestReviewResolvesItsOwnStaleThreads(t *testing.T) {
	gh := newFakeGitHub(t, defaultPR())
	gh.addThread(ghThread{ID: "T-stale", Outdated: true, Comments: []ghThreadComment{
		{Body: "<!-- flynn-review:deadbe -->\n**old-rule** a finding since fixed", Author: "vouchbot[bot]"},
	}})
	gh.addThread(ghThread{ID: "T-human", Outdated: true, Comments: []ghThreadComment{
		{Body: "why is this here?", Author: "a-maintainer"},
	}})

	model := newFakeOpenAIQueue(
		t,
		fetchCall(),
		verdictCall("call-2", "COMMENT"),
		finalText("Left a comment."),
	)
	in := reviewInstance(t, model)

	if res := in.run(reviewArgs(gh)...); res.code != 0 {
		t.Fatalf("review failed: exit %d\n%s", res.code, res.combined())
	}
	got := gh.resolvedThreads()
	if len(got) != 1 || got[0] != "T-stale" {
		t.Fatalf("resolved %v, want exactly [T-stale]: the maintainer's thread is not the reviewer's to close", got)
	}
}

// A finding the reviewer raises again is a finding still open. Its conversation stays.
func TestReviewKeepsAThreadItRaisedAgain(t *testing.T) {
	gh := newFakeGitHub(t, defaultPR())
	model := newFakeOpenAIQueue(
		t,
		fetchCall(),
		commentCall("always-allows"),
		verdictCall("call-3", "REQUEST_CHANGES"),
		finalText("Requested changes."),
	)
	in := reviewInstance(t, model)

	// Run once so the finding's comment exists, then seed its thread with the exact
	// body the binary posted, so the marker is the real one.
	if res := in.run(reviewArgs(gh)...); res.code != 3 {
		t.Fatalf("first review: exit %d\n%s", res.code, res.combined())
	}
	posted := gh.inlineComments()
	if len(posted) != 1 {
		t.Fatalf("want 1 inline comment, got %d", len(posted))
	}
	gh.addThread(ghThread{ID: "T-open", Outdated: true, Comments: []ghThreadComment{
		{Body: posted[0].Body, Author: "vouchbot[bot]"},
	}})

	// A second review raises the same finding. Outdated or not, it is still open.
	model2 := newFakeOpenAIQueue(
		t,
		fetchCall(),
		commentCall("always-allows"),
		verdictCall("call-3", "REQUEST_CHANGES"),
		finalText("Requested changes."),
	)
	in2 := reviewInstance(t, model2)
	if res := in2.run(reviewArgs(gh)...); res.code != 3 {
		t.Fatalf("second review: exit %d\n%s", res.code, res.combined())
	}
	if got := gh.resolvedThreads(); len(got) != 0 {
		t.Fatalf("resolved %v, want none: the finding was raised again", got)
	}
}

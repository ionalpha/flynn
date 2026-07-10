package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeGitHub is an in-process stand-in for the GitHub REST API, serving one pull
// request. It lets the e2e suite drive the real flynn binary through a whole review
// without a network or a credential: the binary is run with --api-base pointed at this
// server, and every write it makes is recorded so a test can assert what reached the
// API rather than what the binary said it did.
//
// The server binds an ephemeral loopback port via httptest. Reaching it exercises the
// command's own egress plumbing: a review's default policy refuses loopback, so a run
// that talks to this server has proved that --api-base widened the policy to the host
// it names.
//
// Writes are stored, not merely counted, and reads serve what earlier writes stored.
// That is what makes the reconcile path testable: a second review of the same pull
// request sees its own first-round comments come back from pr_fetch, so posting the
// same finding twice must update one comment rather than open a second.
type fakeGitHub struct {
	srv *httptest.Server

	mu sync.Mutex
	pr fakePR

	reviewComments []gh // inline comments, in creation order
	issueComments  []gh // top-level comments, in creation order
	reviews        []ghReview
	nextID         int64

	// requests records every method+path the binary asked for, so a test can assert
	// that a refused write never reached the API.
	requests []string

	// fail, when set, answers the request whose method+path it matches with status.
	fail map[string]int
}

// fakePR is the pull request the server serves.
type fakePR struct {
	Number      int
	Title       string
	Body        string
	State       string
	Draft       bool
	AuthorLogin string
	HeadSHA     string
	Files       []fakeFile
	// ChangedFiles overrides the served changed_files count. Zero reports len(Files).
	// A larger value is how a test makes a diff arrive incomplete: the tool compares
	// GitHub's own count against what it fetched.
	ChangedFiles int
}

type fakeFile struct {
	Name      string
	Patch     string
	Additions int
	Deletions int
}

// gh is a stored comment.
type gh struct {
	ID      int64
	Path    string
	Line    int
	Body    string
	HTMLURL string
}

// ghReview is a submitted review verdict.
type ghReview struct {
	Event string
	Body  string
}

// verdictBodies returns the body of each submitted review, in order.
func (f *fakeGitHub) verdictBodies() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.reviews))
	for _, rv := range f.reviews {
		out = append(out, rv.Body)
	}
	return out
}

// defaultPR is a small, clean pull request authored by someone other than the
// reviewer, which most tests review as-is.
func defaultPR() fakePR {
	return fakePR{
		Number:      7,
		Title:       "Add a rate limiter",
		Body:        "Bounds the request rate per client.",
		State:       "open",
		AuthorLogin: "a-contributor",
		HeadSHA:     "d34db33fd34db33fd34db33fd34db33fd34db33f",
		Files: []fakeFile{{
			Name:      "limiter.go",
			Additions: 3,
			Deletions: 0,
			Patch:     "@@ -1,2 +1,5 @@\n package limiter\n+\n+// Allow reports whether the client may proceed.\n+func Allow(client string) bool { return true }\n",
		}},
	}
}

// newFakeGitHub starts a server serving pr. It is torn down at test end.
func newFakeGitHub(t *testing.T, pr fakePR) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{pr: pr, nextID: 1000, fail: map[string]int{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

// baseURL is the value to pass as --api-base.
func (f *fakeGitHub) baseURL() string { return f.srv.URL }

// failOn makes the next and every later request matching "METHOD /path" answer with
// status, so a test can drive the binary's error path on a specific call.
func (f *fakeGitHub) failOn(methodPath string, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail[methodPath] = status
}

var (
	rePR             = regexp.MustCompile(`^/repos/[^/]+/[^/]+/pulls/(\d+)$`)
	rePRFiles        = regexp.MustCompile(`^/repos/[^/]+/[^/]+/pulls/(\d+)/files$`)
	rePRComments     = regexp.MustCompile(`^/repos/[^/]+/[^/]+/pulls/(\d+)/comments$`)
	rePRReviews      = regexp.MustCompile(`^/repos/[^/]+/[^/]+/pulls/(\d+)/reviews$`)
	reIssueComments  = regexp.MustCompile(`^/repos/[^/]+/[^/]+/issues/(\d+)/comments$`)
	reReviewComment  = regexp.MustCompile(`^/repos/[^/]+/[^/]+/pulls/comments/(\d+)$`)
	reIssueCommentID = regexp.MustCompile(`^/repos/[^/]+/[^/]+/issues/comments/(\d+)$`)
)

func (f *fakeGitHub) handle(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	key := r.Method + " " + path

	f.mu.Lock()
	f.requests = append(f.requests, key)
	status, failing := f.fail[key]
	f.mu.Unlock()

	if failing {
		writeGHError(w, status, "scripted failure")
		return
	}

	// The tool must authenticate every request. An unauthenticated one is a bug worth
	// failing loudly on rather than serving.
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		writeGHError(w, http.StatusUnauthorized, "no bearer token")
		return
	}

	switch {
	case r.Method == http.MethodGet && rePR.MatchString(path):
		f.getPR(w)
	case r.Method == http.MethodGet && rePRFiles.MatchString(path):
		f.getFiles(w)
	case r.Method == http.MethodGet && rePRComments.MatchString(path):
		f.listComments(w, &f.reviewComments)
	case r.Method == http.MethodPost && rePRComments.MatchString(path):
		f.createReviewComment(w, r)
	case r.Method == http.MethodGet && reIssueComments.MatchString(path):
		f.listComments(w, &f.issueComments)
	case r.Method == http.MethodPost && reIssueComments.MatchString(path):
		f.createIssueComment(w, r)
	case r.Method == http.MethodPatch && reReviewComment.MatchString(path):
		f.updateComment(w, r, &f.reviewComments, reReviewComment)
	case r.Method == http.MethodPatch && reIssueCommentID.MatchString(path):
		f.updateComment(w, r, &f.issueComments, reIssueCommentID)
	case r.Method == http.MethodPost && rePRReviews.MatchString(path):
		f.submitReview(w, r)
	default:
		writeGHError(w, http.StatusNotFound, "no route for "+key)
	}
}

func (f *fakeGitHub) getPR(w http.ResponseWriter) {
	f.mu.Lock()
	pr := f.pr
	f.mu.Unlock()

	changed := pr.ChangedFiles
	if changed == 0 {
		changed = len(pr.Files)
	}
	var adds, dels int
	for _, file := range pr.Files {
		adds += file.Additions
		dels += file.Deletions
	}
	writeJSON(w, map[string]any{
		"number": pr.Number, "title": pr.Title, "body": pr.Body,
		"state": pr.State, "draft": pr.Draft,
		"changed_files": changed, "additions": adds, "deletions": dels,
		"head": map[string]any{"sha": pr.HeadSHA},
		"user": map[string]any{"login": pr.AuthorLogin},
	})
}

func (f *fakeGitHub) getFiles(w http.ResponseWriter) {
	f.mu.Lock()
	files := f.pr.Files
	f.mu.Unlock()

	out := make([]map[string]any, 0, len(files))
	for _, file := range files {
		out = append(out, map[string]any{
			"filename": file.Name, "status": "modified",
			"additions": file.Additions, "deletions": file.Deletions,
			"patch": file.Patch,
		})
	}
	writeJSON(w, out)
}

func (f *fakeGitHub) listComments(w http.ResponseWriter, store *[]gh) {
	f.mu.Lock()
	out := make([]map[string]any, 0, len(*store))
	for _, c := range *store {
		out = append(out, map[string]any{"id": c.ID, "path": c.Path, "line": c.Line, "body": c.Body, "html_url": c.HTMLURL})
	}
	f.mu.Unlock()
	writeJSON(w, out)
}

func (f *fakeGitHub) createReviewComment(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Body     string `json:"body"`
		Path     string `json:"path"`
		Line     int    `json:"line"`
		CommitID string `json:"commit_id"`
	}
	if !decodeBody(w, r, &in) {
		return
	}
	f.mu.Lock()
	// GitHub anchors an inline comment to a commit. A comment posted against a stale
	// head is a 422 there, so a missing commit_id must not quietly pass here.
	if in.CommitID != f.pr.HeadSHA {
		f.mu.Unlock()
		writeGHError(w, http.StatusUnprocessableEntity, "commit_id is not the pull request head")
		return
	}
	id := f.nextID
	f.nextID++
	f.reviewComments = append(f.reviewComments, gh{
		ID: id, Path: in.Path, Line: in.Line, Body: in.Body,
		HTMLURL: fmt.Sprintf("https://github.com/acme/widgets/pull/7#discussion_r%d", id),
	})
	f.mu.Unlock()
	writeJSON(w, map[string]any{"id": id})
}

func (f *fakeGitHub) createIssueComment(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Body string `json:"body"`
	}
	if !decodeBody(w, r, &in) {
		return
	}
	f.mu.Lock()
	id := f.nextID
	f.nextID++
	f.issueComments = append(f.issueComments, gh{ID: id, Body: in.Body})
	f.mu.Unlock()
	writeJSON(w, map[string]any{"id": id})
}

func (f *fakeGitHub) updateComment(w http.ResponseWriter, r *http.Request, store *[]gh, re *regexp.Regexp) {
	var in struct {
		Body string `json:"body"`
	}
	if !decodeBody(w, r, &in) {
		return
	}
	id, _ := strconv.ParseInt(re.FindStringSubmatch(r.URL.Path)[1], 10, 64)

	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range *store {
		if (*store)[i].ID == id {
			(*store)[i].Body = in.Body
			writeJSON(w, map[string]any{"id": id})
			return
		}
	}
	writeGHError(w, http.StatusNotFound, fmt.Sprintf("no comment %d", id))
}

func (f *fakeGitHub) submitReview(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Event    string `json:"event"`
		Body     string `json:"body"`
		CommitID string `json:"commit_id"`
	}
	if !decodeBody(w, r, &in) {
		return
	}
	f.mu.Lock()
	f.reviews = append(f.reviews, ghReview{Event: in.Event, Body: in.Body})
	f.mu.Unlock()
	writeJSON(w, map[string]any{"id": 1, "state": in.Event})
}

// verdicts returns the review events submitted so far.
func (f *fakeGitHub) verdicts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.reviews))
	for _, rv := range f.reviews {
		out = append(out, rv.Event)
	}
	return out
}

// inlineComments returns the inline review comments currently on the pull request.
func (f *fakeGitHub) inlineComments() []gh {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]gh(nil), f.reviewComments...)
}

// summaries returns the top-level comment bodies currently on the pull request.
func (f *fakeGitHub) summaries() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.issueComments))
	for _, c := range f.issueComments {
		out = append(out, c.Body)
	}
	return out
}

// sawWrite reports whether the binary ever issued a write to the given path suffix.
// A test asserting a refusal uses it to prove the refusal happened before the API
// call, not after it.
func (f *fakeGitHub) sawWrite(suffix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, req := range f.requests {
		method, path, _ := strings.Cut(req, " ")
		if method == http.MethodGet {
			continue
		}
		if strings.HasSuffix(path, suffix) {
			return true
		}
	}
	return false
}

func decodeBody(w http.ResponseWriter, r *http.Request, out any) bool {
	if err := json.NewDecoder(r.Body).Decode(out); err != nil {
		writeGHError(w, http.StatusBadRequest, "undecodable body: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(v)
}

func writeGHError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"message": message})
}

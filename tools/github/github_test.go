package github_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/tools/github"
)

// prPath matches a pull request's own endpoint, for any number, so a test can drive
// more than one pull request through a Set.
var prPath = regexp.MustCompile(`/pulls/(\d+)$`)

// fakeHub is a stand-in GitHub API: enough of the REST surface for the review
// tools, plus counters so a test can assert what was called.
type fakeHub struct {
	t *testing.T

	// clk is the same clock the Set runs on, so an issued token's expiry is
	// expressed in the test's time frame rather than the wall clock's.
	clk clock.Clock

	mu sync.Mutex
	// reviewComments is the pull request's inline comments, keyed by id.
	reviewComments map[int64]string
	// issueComments is the pull request's top-level comments, keyed by id.
	issueComments map[int64]string
	nextID        int64

	prAuthor string
	headSHA  string

	// size counts GitHub reports on the pull request itself.
	changedFiles int
	additions    int
	deletions    int

	// filePages serves paginated /files responses when set.
	filePages [][]map[string]any

	// evilNextLink, when set, is served as the Link header of the first /files page.
	evilNextLink string
	followedEvil atomic.Bool

	// wantAuth, when set, is the exact Authorization header every non-minting request
	// must carry. sawAuth records the last one seen.
	wantAuth string
	sawAuth  atomic.Value

	tokensMinted  atomic.Int64
	created       atomic.Int64
	updated       atomic.Int64
	submittedBody atomic.Value // string: the last review event submitted
	submittedText atomic.Value // string: the last review body submitted

	// threads is what the GraphQL endpoint reports, and resolved records the ids the
	// reviewer asked to close. graphqlError, when set, makes every GraphQL request
	// answer 200 with an errors array, the way GitHub reports a failure.
	threads      []fakeThread
	resolved     []string
	minimized    []string
	graphqlError string
	failReviews  atomic.Bool // when set, a review submission answers 500
	failFiles    atomic.Bool // when set, the changed-files endpoint answers 500
	// failPR answers 500 on the pull request's own endpoint, failCommentList on the
	// inline-comment listing, and failCommentWrite on creating or updating one.
	failPR           atomic.Bool
	failCommentList  atomic.Bool
	failCommentWrite atomic.Bool

	// endlessThreads makes the GraphQL endpoint always report another page, so a test
	// can drive the reviewer into its pagination cap.
	endlessThreads bool

	// commentAuthor is who REST says wrote the inline comments, and viewerLogin is who
	// the credential authenticates as. They match by default, which is the ordinary case
	// of a reviewer reading back its own comments.
	commentAuthor string
	viewerLogin   string

	// commentLine is the line REST reports for an inline comment. GitHub reports null,
	// decoding to zero, once the comment's anchor has left the diff.
	commentLine int

	// commentPaths and commentLines are the anchors of the comments the reviewer posted
	// here, so the listing reports each one where it was actually made rather than at a
	// single fixed coordinate. A comment seeded directly with add carries neither and is
	// reported at the default anchor.
	commentPaths map[int64]string
	commentLines map[int64]int

	// endlessComments makes the inline-comment listing always advertise another page.
	endlessComments bool
}

func newFakeHub(t *testing.T) *fakeHub {
	return &fakeHub{
		t:              t,
		clk:            clock.System{},
		reviewComments: map[int64]string{},
		issueComments:  map[int64]string{},
		commentPaths:   map[int64]string{},
		commentLines:   map[int64]int{},
		nextID:         1000,
		prAuthor:       "someone-else",
		headSHA:        "deadbeef",
		changedFiles:   1,
		additions:      1,
		deletions:      0,
		commentAuthor:  "reviewer[bot]",
		viewerLogin:    "reviewer[bot]",
		commentLine:    1,
	}
}

func (h *fakeHub) add(kind, body string) int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nextID++
	if kind == "review" {
		h.reviewComments[h.nextID] = body
	} else {
		h.issueComments[h.nextID] = body
	}
	return h.nextID
}

// anchor records where a created comment was posted, taken from the request the
// reviewer sent, so the listing can report it back at that path and line.
func (h *fakeHub) anchor(id int64, in map[string]any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if path, ok := in["path"].(string); ok {
		h.commentPaths[id] = path
	}
	if line, ok := in["line"].(float64); ok {
		h.commentLines[id] = int(line)
	}
}

// anchorOf reports where the listing places a comment. A comment the reviewer posted
// is reported where it posted it; one seeded directly sits at the default anchor. A
// hub with commentLine zero reports every line as null, which is how GitHub reports a
// comment whose anchor has left the diff. Callers hold h.mu.
func (h *fakeHub) anchorOf(id int64) (string, int) {
	path, line := "a.go", h.commentLine
	if p, ok := h.commentPaths[id]; ok {
		path = p
	}
	if l, ok := h.commentLines[id]; ok && h.commentLine > 0 {
		line = l
	}
	return path, line
}

func (h *fakeHub) bodies(kind string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	src := h.reviewComments
	if kind == "issue" {
		src = h.issueComments
	}
	out := make([]string, 0, len(src))
	for _, b := range src {
		out = append(out, b)
	}
	return out
}

func (h *fakeHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	if auth := r.Header.Get("Authorization"); auth != "" {
		h.sawAuth.Store(auth)
		if h.wantAuth != "" && !strings.HasSuffix(p, "/access_tokens") && auth != h.wantAuth {
			h.t.Errorf("Authorization = %q, want %q", auth, h.wantAuth)
		}
	}
	switch {
	case strings.HasSuffix(p, "/graphql") && r.Method == http.MethodPost:
		h.serveGraphQL(w, r)

	case strings.HasSuffix(p, "/access_tokens"):
		h.tokensMinted.Add(1)
		if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "Bearer ey") {
			h.t.Errorf("assertion header not a JWT: %q", got)
		}
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, map[string]any{
			"token":      "ghs_installation_token",
			"expires_at": h.clk.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		})

	case strings.HasSuffix(p, "/files"):
		if h.failFiles.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		h.serveFiles(w, r)

	case strings.HasSuffix(p, "/reviews") && r.Method == http.MethodPost:
		if h.failReviews.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		var in map[string]any
		decode(h.t, r, &in)
		h.submittedBody.Store(fmt.Sprint(in["event"]))
		h.submittedText.Store(fmt.Sprint(in["body"]))
		w.WriteHeader(http.StatusOK)
		writeJSON(w, map[string]any{"id": 1})

	case strings.HasSuffix(p, "/pulls/7/comments") && r.Method == http.MethodPost:
		if h.failCommentWrite.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		var in map[string]any
		decode(h.t, r, &in)
		h.created.Add(1)
		id := h.add("review", fmt.Sprint(in["body"]))
		h.anchor(id, in)
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, map[string]any{
			"id":       id,
			"html_url": fmt.Sprintf("https://github.com/o/r/pull/7#discussion_r%d", id),
		})

	case strings.HasSuffix(p, "/pulls/7/comments") && r.Method == http.MethodGet && h.failCommentList.Load():
		w.WriteHeader(http.StatusInternalServerError)

	case strings.HasSuffix(p, "/pulls/7/comments") && r.Method == http.MethodGet && h.endlessComments:
		// Always another page, so a test can drive the client into its pagination cap.
		w.Header().Set("Link", "<http://"+r.Host+"/repos/ionalpha/flynn/pulls/7/comments?page=2>; rel=\"next\"")
		writeJSON(w, []map[string]any{})

	case strings.HasSuffix(p, "/pulls/7/comments") && r.Method == http.MethodGet:
		h.mu.Lock()
		out := []map[string]any{}
		for id, b := range h.reviewComments {
			path, line := h.anchorOf(id)
			out = append(out, map[string]any{
				"id": id, "body": b, "path": path, "line": line,
				"html_url": fmt.Sprintf("https://github.com/o/r/pull/7#discussion_r%d", id),
				// REST reports the App with its suffix, and the viewer query answers the same.
				"user": map[string]any{"login": h.commentAuthor},
			})
		}
		h.mu.Unlock()
		writeJSON(w, out)

	case strings.Contains(p, "/pulls/comments/") && r.Method == http.MethodPatch:
		if h.failCommentWrite.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		h.updated.Add(1)
		w.WriteHeader(http.StatusOK)
		writeJSON(w, map[string]any{"id": 1, "html_url": "https://github.com/o/r/pull/7#discussion_r1"})

	case strings.HasSuffix(p, "/issues/7/comments") && r.Method == http.MethodPost:
		var in map[string]any
		decode(h.t, r, &in)
		h.add("issue", fmt.Sprint(in["body"]))
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, map[string]any{"id": 1})

	case strings.HasSuffix(p, "/issues/7/comments") && r.Method == http.MethodGet:
		h.mu.Lock()
		out := []map[string]any{}
		for id, b := range h.issueComments {
			out = append(out, map[string]any{"id": id, "body": b})
		}
		h.mu.Unlock()
		writeJSON(w, out)

	case strings.Contains(p, "/issues/comments/") && r.Method == http.MethodPatch:
		var in map[string]any
		decode(h.t, r, &in)
		h.updated.Add(1)
		w.WriteHeader(http.StatusOK)
		writeJSON(w, map[string]any{"id": 1})

	case prPath.MatchString(p) && h.failPR.Load():
		w.WriteHeader(http.StatusInternalServerError)

	case prPath.MatchString(p):
		n, _ := strconv.Atoi(prPath.FindStringSubmatch(p)[1])
		writeJSON(w, map[string]any{
			"number": n, "title": "t", "body": "b", "state": "open", "draft": false,
			"changed_files": h.changedFiles, "additions": h.additions, "deletions": h.deletions,
			"head": map[string]any{"sha": h.headSHA},
			"user": map[string]any{"login": h.prAuthor},
		})

	default:
		h.t.Errorf("unexpected request: %s %s", r.Method, p)
		w.WriteHeader(http.StatusNotFound)
	}
}

func (h *fakeHub) serveFiles(w http.ResponseWriter, r *http.Request) {
	if h.evilNextLink != "" {
		if r.URL.Query().Get("page") != "" {
			h.followedEvil.Store(true)
		}
		w.Header().Set("Link", h.evilNextLink)
		writeJSON(w, []map[string]any{{"filename": "a.go", "patch": "p"}})
		return
	}
	if h.filePages == nil {
		writeJSON(w, []map[string]any{
			{"filename": "a.go", "status": "modified", "additions": 1, "deletions": 0, "patch": "@@ -1 +1 @@\n-a\n+b"},
		})
		return
	}
	page := 0
	if p := r.URL.Query().Get("page"); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil {
			h.t.Fatalf("bad page parameter %q: %v", p, err)
		}
		page = n - 1
	}
	if page < len(h.filePages)-1 {
		next := fmt.Sprintf("<%s://%s%s?per_page=100&page=%d>; rel=\"next\"",
			schemeOf(r), r.Host, r.URL.Path, page+2)
		w.Header().Set("Link", next)
	}
	writeJSON(w, h.filePages[page])
}

func schemeOf(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

// fakeThread is one review thread as the GraphQL endpoint reports it.
type fakeThread struct {
	id         string
	resolved   bool
	outdated   bool
	canResolve bool
	comments   []fakeThreadComment
}

type fakeThreadComment struct {
	body   string
	author string
}

// addThread registers a review thread for the GraphQL endpoint to report.
func (h *fakeHub) addThread(t fakeThread) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.threads = append(h.threads, t)
}

// resolvedThreads returns the ids the reviewer asked GitHub to close.
func (h *fakeHub) resolvedThreads() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.resolved...)
}

// minimizedComments returns the comment ids the reviewer asked GitHub to fold away.
func (h *fakeHub) minimizedComments() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.minimized...)
}

// serveGraphQL answers the two operations the reviewer issues: reading the review
// threads, and resolving one. It matches on the query text rather than parsing
// GraphQL, which is enough to tell a read from a write.
func (h *fakeHub) serveGraphQL(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	decode(h.t, r, &in)

	h.mu.Lock()
	defer h.mu.Unlock()

	if h.graphqlError != "" {
		// GitHub answers a failed GraphQL request with 200 and an errors array.
		writeJSON(w, map[string]any{"errors": []map[string]any{{"message": h.graphqlError}}})
		return
	}

	if strings.Contains(in.Query, "viewer { login") {
		writeJSON(w, map[string]any{"data": map[string]any{
			"viewer": map[string]any{"login": h.viewerLogin},
		}})
		return
	}

	if strings.Contains(in.Query, "minimizeComment") {
		id, _ := in.Variables["subjectId"].(string)
		h.minimized = append(h.minimized, id)
		writeJSON(w, map[string]any{"data": map[string]any{
			"minimizeComment": map[string]any{"minimizedComment": map[string]any{"isMinimized": true}},
		}})
		return
	}

	if strings.Contains(in.Query, "resolveReviewThread") {
		id, _ := in.Variables["threadId"].(string)
		h.resolved = append(h.resolved, id)
		for i := range h.threads {
			if h.threads[i].id == id {
				h.threads[i].resolved = true
			}
		}
		writeJSON(w, map[string]any{"data": map[string]any{
			"resolveReviewThread": map[string]any{"thread": map[string]any{"id": id, "isResolved": true}},
		}})
		return
	}

	// One thread per page, so a test that seeds two threads proves the reviewer follows
	// the cursor. GitHub returns 100 per page; the count is not what is under test.
	start := 0
	if cursor, ok := in.Variables["after"].(string); ok && cursor != "" {
		start, _ = strconv.Atoi(cursor)
	}
	nodes := make([]map[string]any, 0, 1)
	if start < len(h.threads) {
		t := h.threads[start]
		comments := make([]map[string]any, 0, len(t.comments))
		for i, c := range t.comments {
			comments = append(comments, map[string]any{
				"id": fmt.Sprintf("%s-c%d", t.id, i), "body": c.body,
				"author": map[string]any{"login": c.author},
			})
		}
		nodes = append(nodes, map[string]any{
			"id": t.id, "isResolved": t.resolved, "isOutdated": t.outdated,
			"viewerCanResolve": t.canResolve,
			"comments":         map[string]any{"nodes": comments},
		})
	}
	next := start + 1
	hasNext := next < len(h.threads)
	if h.endlessThreads {
		hasNext = true
	}
	writeJSON(w, map[string]any{"data": map[string]any{
		"repository": map[string]any{"pullRequest": map[string]any{
			"reviewThreads": map[string]any{
				"pageInfo": map[string]any{
					"hasNextPage": hasNext,
					"endCursor":   strconv.Itoa(next),
				},
				"nodes": nodes,
			},
		}},
	}})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func decode(t *testing.T, r *http.Request, out any) {
	t.Helper()
	if err := json.NewDecoder(r.Body).Decode(out); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
}

// newSet wires a Set at a fake hub. The key is generated per test; 2048 bits keeps
// the test fast while exercising the real RS256 signing path.
func newSet(t *testing.T, hub *fakeHub, mutate func(*github.Config)) *github.Set {
	t.Helper()
	srv := httptest.NewServer(hub)
	t.Cleanup(srv.Close)

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	cfg := github.Config{
		App:        github.App{Issuer: "Iv1.test", InstallationID: 42, PrivateKey: key},
		Owner:      "ionalpha",
		Repo:       "flynn",
		Number:     7,
		SelfLogin:  "reviewer[bot]",
		HTTPClient: srv.Client(),
		APIBase:    srv.URL,
		Clock:      clock.NewManual(time.Unix(1_700_000_000, 0).UTC()),
	}
	if mutate != nil {
		mutate(&cfg)
	}
	hub.clk = cfg.Clock
	set, err := github.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return set
}

func toolNamed(t *testing.T, s *github.Set, name string) mission.Tool {
	t.Helper()
	for _, tl := range s.Tools() {
		if tl.Def().Name == name {
			return tl
		}
	}
	t.Fatalf("tool %q not registered", name)
	return nil
}

func invoke(t *testing.T, tl mission.Tool, input string) (string, error) {
	t.Helper()
	return tl.Invoke(context.Background(), json.RawMessage(input))
}

// --- the approve gate --------------------------------------------------------

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

// --- idempotency -------------------------------------------------------------

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

// --- evidence discipline -----------------------------------------------------

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

// --- fetch -------------------------------------------------------------------

func TestPRFetchReturnsDiffAndPostedMarkers(t *testing.T) {
	hub := newFakeHub(t)
	set := newSet(t, hub, nil)

	// Seed a comment the reviewer previously posted.
	tool := toolNamed(t, set, "github_comment")
	if _, err := invoke(t, tool, `{"findings":[{"path":"a.go","line":12,"rule":"r","summary":"s","failure":"f"}]}`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	out, err := invoke(t, toolNamed(t, set, "github_pr_fetch"), `{}`)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	var res struct {
		Number         int    `json:"number"`
		AuthorLogin    string `json:"author_login"`
		HeadSHA        string `json:"head_sha"`
		PostedFindings []struct {
			Path    string `json:"path"`
			Line    int    `json:"line"`
			Rule    string `json:"rule"`
			Summary string `json:"summary"`
			Failure string `json:"failure"`
		} `json:"posted_findings"`
		Files []struct {
			Filename string `json:"filename"`
			Patch    string `json:"patch"`
		} `json:"files"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if res.Number != 7 || res.HeadSHA != "deadbeef" || res.AuthorLogin != "someone-else" {
		t.Fatalf("metadata wrong: %+v", res)
	}
	if len(res.Files) != 1 || res.Files[0].Filename != "a.go" || !strings.Contains(res.Files[0].Patch, "+b") {
		t.Fatalf("diff wrong: %+v", res.Files)
	}
	// A finding already on the pull request is reported with the whole claim it made:
	// path, line, and rule to repost it in place, and the summary and failure so the
	// reviewer can re-read what it said and judge the current diff against it rather than
	// restate a bare location on faith.
	if len(res.PostedFindings) != 1 {
		t.Fatalf("posted findings = %v, want exactly 1", res.PostedFindings)
	}
	got := res.PostedFindings[0]
	if got.Path == "" || got.Line == 0 || got.Rule == "" {
		t.Fatalf("posted finding %+v cannot be posted again: it names no path, line, or rule", got)
	}
	if got.Summary == "" || got.Failure == "" {
		t.Fatalf("posted finding %+v carries no claim to re-judge: summary or failure is empty", got)
	}
}

// Two findings can share a path and a line and differ only in their rule. Each keys a
// conversation of its own, so both are reported back, and their order cannot be left to
// the sort: a pull request nothing has happened to must read the same way twice.
func TestPostedFindingsOnOneLineAreOrderedByRule(t *testing.T) {
	hub := newFakeHub(t)
	hub.commentLine = 12
	set := newSet(t, hub, nil)

	// Seeded worst-first, so an order that merely preserves the input would come back
	// wrong.
	seed := `{"findings":[
      {"path":"a.go","line":12,"rule":"zeta","summary":"s","failure":"f"},
      {"path":"a.go","line":12,"rule":"alpha","summary":"s","failure":"f"}]}`
	if _, err := invoke(t, toolNamed(t, set, "github_comment"), seed); err != nil {
		t.Fatalf("seed: %v", err)
	}

	out, err := invoke(t, toolNamed(t, set, "github_pr_fetch"), `{}`)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	var res struct {
		PostedFindings []github.PostedFinding `json:"posted_findings"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := []github.PostedFinding{
		{Path: "a.go", Line: 12, Rule: "alpha", Summary: "s", Failure: "f"},
		{Path: "a.go", Line: 12, Rule: "zeta", Summary: "s", Failure: "f"},
	}
	if !reflect.DeepEqual(res.PostedFindings, want) {
		t.Fatalf("posted findings = %+v, want %+v", res.PostedFindings, want)
	}
}

func TestPRFetchFollowsPaginationAndCapsFiles(t *testing.T) {
	hub := newFakeHub(t)
	hub.filePages = [][]map[string]any{
		{{"filename": "a.go", "patch": "p"}, {"filename": "b.go", "patch": "p"}},
		{{"filename": "c.go", "patch": "p"}, {"filename": "d.go", "patch": "p"}},
	}
	set := newSet(t, hub, func(c *github.Config) { c.MaxFiles = 3 })

	out, err := invoke(t, toolNamed(t, set, "github_pr_fetch"), `{}`)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	var res struct {
		Files          []struct{ Filename string } `json:"files"`
		FilesTruncated bool                        `json:"files_truncated"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(res.Files) != 3 {
		t.Fatalf("files = %d, want 3 (the cap)", len(res.Files))
	}
	if !res.FilesTruncated {
		t.Fatal("truncation must be reported, not silent")
	}
}

func TestPatchTruncationIsFlaggedAndCutOnALineBoundary(t *testing.T) {
	hub := newFakeHub(t)
	hub.filePages = [][]map[string]any{{
		{"filename": "big.go", "patch": "line one\nline two\nline three\n"},
	}}
	set := newSet(t, hub, func(c *github.Config) { c.MaxPatchBytes = 14 })

	out, err := invoke(t, toolNamed(t, set, "github_pr_fetch"), `{}`)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	var res struct {
		Files []struct {
			Patch          string `json:"patch"`
			PatchTruncated bool   `json:"patch_truncated"`
		} `json:"files"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	f := res.Files[0]
	if !f.PatchTruncated {
		t.Fatal("truncation must be flagged")
	}
	// The cut lands on the line boundary and keeps the newline, so a model reads whole
	// lines of diff or none of a line.
	if f.Patch != "line one\n" {
		t.Fatalf("patch = %q, want %q", f.Patch, "line one\n")
	}
}

// A patch whose only newline is its first byte has a boundary at index zero, and the
// cut must land on it. This is the case that was cut mid-line: the boundary search
// skipped index zero, so the result carried a partial second line.
func TestPatchTruncationHandlesALeadingNewline(t *testing.T) {
	hub := newFakeHub(t)
	hub.filePages = [][]map[string]any{{
		{"filename": "odd.go", "patch": "\nABCDEF"},
	}}
	set := newSet(t, hub, func(c *github.Config) { c.MaxPatchBytes = 3 })

	out, err := invoke(t, toolNamed(t, set, "github_pr_fetch"), `{}`)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	var res struct {
		Files []struct {
			Patch          string `json:"patch"`
			PatchTruncated bool   `json:"patch_truncated"`
		} `json:"files"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := res.Files[0].Patch; got != "\n" {
		t.Fatalf("patch = %q, want %q: the cut must land on the boundary at index zero", got, "\n")
	}
	if !res.Files[0].PatchTruncated {
		t.Fatal("truncation must be flagged")
	}
}

// --- auth --------------------------------------------------------------------

// The installation token is minted once and reused while valid, so a review does
// not mint a token per API call.
func TestInstallationTokenIsCached(t *testing.T) {
	hub := newFakeHub(t)
	set := newSet(t, hub, nil)

	for range 3 {
		if _, err := invoke(t, toolNamed(t, set, "github_pr_fetch"), `{}`); err != nil {
			t.Fatalf("fetch: %v", err)
		}
	}
	if got := hub.tokensMinted.Load(); got != 1 {
		t.Fatalf("tokens minted = %d, want 1 (the cache is not working)", got)
	}
}

// A token near expiry is refreshed rather than handed to a request that outlives it.
func TestInstallationTokenRefreshesBeforeExpiry(t *testing.T) {
	hub := newFakeHub(t)
	clk := clock.NewManual(time.Unix(1_700_000_000, 0).UTC())
	set := newSet(t, hub, func(c *github.Config) { c.Clock = clk })

	if _, err := invoke(t, toolNamed(t, set, "github_pr_fetch"), `{}`); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got := hub.tokensMinted.Load(); got != 1 {
		t.Fatalf("tokens minted = %d, want 1", got)
	}

	// The hub issues tokens expiring an hour from the test clock; advance past it.
	clk.Advance(2 * time.Hour)
	if _, err := invoke(t, toolNamed(t, set, "github_pr_fetch"), `{}`); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got := hub.tokensMinted.Load(); got != 2 {
		t.Fatalf("tokens minted = %d, want 2 (an expired token was reused)", got)
	}
}

// --- construction ------------------------------------------------------------

func TestNewRejectsIncompleteConfig(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	full := func() github.Config {
		return github.Config{
			App:   github.App{Issuer: "i", InstallationID: 1, PrivateKey: key},
			Owner: "o", Repo: "r", Number: 7,
		}
	}
	cases := map[string]func(*github.Config){
		"no owner":        func(c *github.Config) { c.Owner = "" },
		"no repo":         func(c *github.Config) { c.Repo = "" },
		"no number":       func(c *github.Config) { c.Number = 0 },
		"no installation": func(c *github.Config) { c.App.InstallationID = 0 },
		"no private key":  func(c *github.Config) { c.App.PrivateKey = nil },
	}
	for name, invalidate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := full()
			invalidate(&cfg)
			if _, err := github.New(cfg); err == nil {
				t.Fatal("want an error")
			}
		})
	}
	if _, err := github.New(full()); err != nil {
		t.Fatalf("a complete config must build: %v", err)
	}
}

// The toolset exposes exactly the three review capabilities and nothing else. A
// reviewer's authority is the tools it holds, so an accidental addition here is a
// widening of authority.
func TestToolsetSurfaceIsExactlyTheReviewCapabilities(t *testing.T) {
	hub := newFakeHub(t)
	set := newSet(t, hub, nil)

	want := map[string]bool{"github_pr_fetch": true, "github_comment": true, "github_submit_review": true}
	got := map[string]bool{}
	for _, tl := range set.Tools() {
		got[tl.Def().Name] = true
	}
	if len(got) != len(want) {
		t.Fatalf("toolset = %v, want exactly %v", got, want)
	}
	for name := range want {
		if !got[name] {
			t.Errorf("missing tool %q", name)
		}
	}
}

// --- review budget and diff completeness -------------------------------------

// A runaway diff is refused before a single model token is spent on it.
func TestFetchRefusesOversizeDiff(t *testing.T) {
	hub := newFakeHub(t)
	hub.additions, hub.deletions = 9000, 1000
	set := newSet(t, hub, func(c *github.Config) { c.MaxChangedLines = 3000 })

	_, err := invoke(t, toolNamed(t, set, "github_pr_fetch"), `{}`)
	if !errors.Is(err, github.ErrDiffTooLarge) {
		t.Fatalf("want ErrDiffTooLarge, got %v", err)
	}
}

// The explicit "review the big one anyway" path is an opt-in, not a default.
func TestFetchReviewsOversizeWhenExplicitlyAllowed(t *testing.T) {
	hub := newFakeHub(t)
	hub.additions, hub.deletions = 9000, 1000
	set := newSet(t, hub, func(c *github.Config) {
		c.MaxChangedLines = 3000
		c.ReviewOversize = true
	})

	out, err := invoke(t, toolNamed(t, set, "github_pr_fetch"), `{}`)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	var res struct {
		ChangedLines int `json:"changed_lines"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if res.ChangedLines != 10000 {
		t.Fatalf("changed_lines = %d, want 10000", res.ChangedLines)
	}
}

// A negative budget disables the check entirely.
func TestNegativeBudgetDisablesTheLimit(t *testing.T) {
	hub := newFakeHub(t)
	hub.additions, hub.deletions = 500000, 0
	set := newSet(t, hub, func(c *github.Config) { c.MaxChangedLines = -1 })

	if _, err := invoke(t, toolNamed(t, set, "github_pr_fetch"), `{}`); err != nil {
		t.Fatalf("fetch: %v", err)
	}
}

// The budget is measured against GitHub's own total, so a diff cannot slip under it
// by being truncated on the way in.
func TestBudgetUsesAuthoritativeCountsNotTheFetchedDiff(t *testing.T) {
	hub := newFakeHub(t)
	hub.additions, hub.deletions = 50000, 0                                  // huge PR ...
	hub.filePages = [][]map[string]any{{{"filename": "a.go", "patch": "p"}}} // ... tiny fetched diff
	set := newSet(t, hub, func(c *github.Config) { c.MaxChangedLines = 100 })

	if _, err := invoke(t, toolNamed(t, set, "github_pr_fetch"), `{}`); !errors.Is(err, github.ErrDiffTooLarge) {
		t.Fatalf("want ErrDiffTooLarge from the authoritative count, got %v", err)
	}
}

// Approving a diff that was never fully read is a false claim. No configuration
// relaxes this, not even AllowApprove.
func TestApproveRefusedWhenFileListTruncated(t *testing.T) {
	hub := newFakeHub(t)
	hub.changedFiles = 10 // more files than the fetch cap
	set := newSet(t, hub, func(c *github.Config) {
		c.AllowApprove = true
		c.MaxFiles = 2
	})

	_, err := invoke(t, toolNamed(t, set, "github_submit_review"), `{"event":"APPROVE","conclusion":"Nothing blocking."}`)
	if !errors.Is(err, github.ErrIncompleteDiff) {
		t.Fatalf("want ErrIncompleteDiff, got %v", err)
	}
	if v := hub.submittedBody.Load(); v != nil {
		t.Fatalf("an approval on a truncated diff was submitted: %v", v)
	}
}

func TestApproveRefusedWhenAPatchWasTruncated(t *testing.T) {
	hub := newFakeHub(t)
	hub.filePages = [][]map[string]any{{
		{"filename": "big.go", "patch": "line one\nline two\nline three\n"},
	}}
	set := newSet(t, hub, func(c *github.Config) {
		c.AllowApprove = true
		c.MaxPatchBytes = 12
	})

	_, err := invoke(t, toolNamed(t, set, "github_submit_review"), `{"event":"APPROVE","conclusion":"Nothing blocking."}`)
	if !errors.Is(err, github.ErrIncompleteDiff) {
		t.Fatalf("want ErrIncompleteDiff, got %v", err)
	}
}

// Seeing one real defect is enough to say no; seeing most of a diff is never enough
// to say yes. Blocking stays available on partial evidence.
func TestRequestChangesAllowedOnTruncatedDiff(t *testing.T) {
	hub := newFakeHub(t)
	hub.changedFiles = 10
	set := newSet(t, hub, func(c *github.Config) { c.MaxFiles = 2 })

	if _, err := invoke(t, toolNamed(t, set, "github_submit_review"), `{"event":"REQUEST_CHANGES","conclusion":"Findings need addressing."}`); err != nil {
		t.Fatalf("REQUEST_CHANGES on a partial diff must be allowed: %v", err)
	}
	if got := hub.submittedBody.Load(); got != "REQUEST_CHANGES" {
		t.Fatalf("event = %v, want REQUEST_CHANGES", got)
	}
}

// A complete diff approves normally, which guards against the gate refusing
// everything.
func TestApproveAllowedOnCompleteDiff(t *testing.T) {
	hub := newFakeHub(t)
	set := newSet(t, hub, func(c *github.Config) { c.AllowApprove = true })

	if _, err := invoke(t, toolNamed(t, set, "github_submit_review"), `{"event":"APPROVE","conclusion":"Nothing blocking."}`); err != nil {
		t.Fatalf("approve on a complete diff: %v", err)
	}
	if got := hub.submittedBody.Load(); got != "APPROVE" {
		t.Fatalf("event = %v, want APPROVE", got)
	}
}

// A force-reviewed oversize pull request may still be approved, provided the diff
// itself arrived intact: the budget is about cost, completeness is about truth.
func TestOversizeButCompleteDiffCanBeApproved(t *testing.T) {
	hub := newFakeHub(t)
	hub.additions, hub.deletions = 9000, 1000
	set := newSet(t, hub, func(c *github.Config) {
		c.AllowApprove = true
		c.MaxChangedLines = 3000
		c.ReviewOversize = true
	})

	if _, err := invoke(t, toolNamed(t, set, "github_submit_review"), `{"event":"APPROVE","conclusion":"Nothing blocking."}`); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if got := hub.submittedBody.Load(); got != "APPROVE" {
		t.Fatalf("event = %v, want APPROVE", got)
	}
}

func TestFetchReportsDiffCompleteness(t *testing.T) {
	hub := newFakeHub(t)
	hub.changedFiles = 10
	set := newSet(t, hub, func(c *github.Config) { c.MaxFiles = 2 })

	out, err := invoke(t, toolNamed(t, set, "github_pr_fetch"), `{}`)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	var res struct {
		DiffComplete bool `json:"diff_complete"`
		ChangedFiles int  `json:"changed_files"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if res.DiffComplete {
		t.Fatal("diff_complete must be false when more files changed than the cap allows")
	}
	if res.ChangedFiles != 10 {
		t.Fatalf("changed_files = %d, want 10", res.ChangedFiles)
	}
}

// --- pagination safety -------------------------------------------------------

// The Link header is chosen by whoever answered the request, while the credential
// attached to the next request is ours. A next-page link pointing at another host
// must be refused, not followed with an installation token in hand.
func TestPaginationLinkOffTheAPIHostIsRefused(t *testing.T) {
	hub := newFakeHub(t)
	hub.evilNextLink = `<https://attacker.example/steal>; rel="next"`
	set := newSet(t, hub, nil)

	_, err := invoke(t, toolNamed(t, set, "github_pr_fetch"), `{}`)
	if err == nil {
		t.Fatal("a cross-host pagination link must be refused")
	}
	if !strings.Contains(err.Error(), "leaves the API host") {
		t.Fatalf("error = %v, want it to name the off-host link", err)
	}
	if hub.followedEvil.Load() {
		t.Fatal("the off-host link was followed with a token attached")
	}
}

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

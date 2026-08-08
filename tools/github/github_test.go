package github_test

// The fake GitHub for this package's external tests: enough of the REST surface and
// the two GraphQL operations the reviewer issues, plus counters so a test can assert
// what was called. Nothing here reaches the network. The tests themselves sit in the
// subject files alongside this one, all of them built on newSet and this hub.

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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

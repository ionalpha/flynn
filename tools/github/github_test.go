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

	tokensMinted  atomic.Int64
	created       atomic.Int64
	updated       atomic.Int64
	submittedBody atomic.Value // string: the last review event submitted
}

func newFakeHub(t *testing.T) *fakeHub {
	return &fakeHub{
		t:              t,
		clk:            clock.System{},
		reviewComments: map[int64]string{},
		issueComments:  map[int64]string{},
		nextID:         1000,
		prAuthor:       "someone-else",
		headSHA:        "deadbeef",
		changedFiles:   1,
		additions:      1,
		deletions:      0,
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
	switch {
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
		h.serveFiles(w, r)

	case strings.HasSuffix(p, "/reviews") && r.Method == http.MethodPost:
		var in map[string]any
		decode(h.t, r, &in)
		h.submittedBody.Store(fmt.Sprint(in["event"]))
		w.WriteHeader(http.StatusOK)
		writeJSON(w, map[string]any{"id": 1})

	case strings.HasSuffix(p, "/pulls/7/comments") && r.Method == http.MethodPost:
		var in map[string]any
		decode(h.t, r, &in)
		h.created.Add(1)
		h.add("review", fmt.Sprint(in["body"]))
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, map[string]any{"id": 1})

	case strings.HasSuffix(p, "/pulls/7/comments") && r.Method == http.MethodGet:
		h.mu.Lock()
		out := []map[string]any{}
		for id, b := range h.reviewComments {
			out = append(out, map[string]any{"id": id, "body": b, "path": "a.go", "line": 1})
		}
		h.mu.Unlock()
		writeJSON(w, out)

	case strings.Contains(p, "/pulls/comments/") && r.Method == http.MethodPatch:
		h.updated.Add(1)
		w.WriteHeader(http.StatusOK)
		writeJSON(w, map[string]any{"id": 1})

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

	case strings.HasSuffix(p, "/pulls/7"):
		writeJSON(w, map[string]any{
			"number": 7, "title": "t", "body": "b", "state": "open", "draft": false,
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

	_, err := invoke(t, toolNamed(t, set, "github_submit_review"), `{"number":7,"event":"APPROVE"}`)
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

	if _, err := invoke(t, toolNamed(t, set, "github_submit_review"), `{"number":7,"event":"APPROVE"}`); err != nil {
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

	_, err := invoke(t, toolNamed(t, set, "github_submit_review"), `{"number":7,"event":"APPROVE"}`)
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

	if _, err := invoke(t, toolNamed(t, set, "github_submit_review"), `{"number":7,"event":"REQUEST_CHANGES"}`); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if got := hub.submittedBody.Load(); got != "REQUEST_CHANGES" {
		t.Fatalf("event = %v, want REQUEST_CHANGES", got)
	}
}

func TestSubmitReviewRejectsUnknownEvent(t *testing.T) {
	hub := newFakeHub(t)
	set := newSet(t, hub, nil)

	if _, err := invoke(t, toolNamed(t, set, "github_submit_review"), `{"number":7,"event":"LGTM"}`); err == nil {
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

	const findings = `{"number":7,"findings":[
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

	first := `{"number":7,"findings":[{"path":"a.go","line":12,"rule":"nil-deref","summary":"cfg may be nil","failure":"panics"}]}`
	second := `{"number":7,"findings":[{"path":"a.go","line":12,"rule":"nil-deref","summary":"cfg is nil on the error path","failure":"cfg=nil panics"}]}`

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

	in := `{"number":7,"findings":[
      {"path":"a.go","line":12,"rule":"r","summary":"s","failure":"f"},
      {"path":"a.go","line":30,"rule":"r","summary":"s","failure":"f"}]}`
	if _, err := invoke(t, tool, in); err != nil {
		t.Fatalf("post: %v", err)
	}
	if got := hub.created.Load(); got != 2 {
		t.Fatalf("created = %d, want 2", got)
	}
}

// The summary is sticky: posted once, then rewritten in place.
func TestSummaryIsStickyAndUpdatedInPlace(t *testing.T) {
	hub := newFakeHub(t)
	set := newSet(t, hub, nil)
	tool := toolNamed(t, set, "github_comment")

	if _, err := invoke(t, tool, `{"number":7,"summary":"first pass"}`); err != nil {
		t.Fatalf("first: %v", err)
	}
	if n := len(hub.bodies("issue")); n != 1 {
		t.Fatalf("issue comments = %d, want 1", n)
	}
	if _, err := invoke(t, tool, `{"number":7,"summary":"second pass"}`); err != nil {
		t.Fatalf("second: %v", err)
	}
	if n := len(hub.bodies("issue")); n != 1 {
		t.Fatalf("issue comments = %d after re-run, want 1 (summary duplicated)", n)
	}
	if got := hub.updated.Load(); got != 1 {
		t.Fatalf("updated = %d, want 1", got)
	}
}

// A human's comment carries no marker and must never be adopted or overwritten.
func TestReconcileIgnoresHumanComments(t *testing.T) {
	hub := newFakeHub(t)
	hub.add("review", "I think this is fine actually")
	hub.add("issue", "thanks for the patch!")
	set := newSet(t, hub, nil)
	tool := toolNamed(t, set, "github_comment")

	in := `{"number":7,"summary":"sum","findings":[{"path":"a.go","line":12,"rule":"r","summary":"s","failure":"f"}]}`
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
		"no failure": `{"number":7,"findings":[{"path":"a.go","line":1,"rule":"r","summary":"s","failure":"  "}]}`,
		"no summary": `{"number":7,"findings":[{"path":"a.go","line":1,"rule":"r","summary":"","failure":"f"}]}`,
		"no rule":    `{"number":7,"findings":[{"path":"a.go","line":1,"rule":"","summary":"s","failure":"f"}]}`,
		"no line":    `{"number":7,"findings":[{"path":"a.go","line":0,"rule":"r","summary":"s","failure":"f"}]}`,
		"no path":    `{"number":7,"findings":[{"path":"","line":1,"rule":"r","summary":"s","failure":"f"}]}`,
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

	in := `{"number":7,"findings":[
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
	if _, err := invoke(t, tool, `{"number":7,"findings":[{"path":"a.go","line":12,"rule":"r","summary":"s","failure":"f"}]}`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	out, err := invoke(t, toolNamed(t, set, "github_pr_fetch"), `{"number":7}`)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	var res struct {
		Number         int      `json:"number"`
		AuthorLogin    string   `json:"author_login"`
		HeadSHA        string   `json:"head_sha"`
		PostedFindings []string `json:"posted_findings"`
		Files          []struct {
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
	// The already-posted finding is reported so the model does not re-propose it.
	if len(res.PostedFindings) != 1 {
		t.Fatalf("posted findings = %v, want exactly 1", res.PostedFindings)
	}
}

func TestPRFetchFollowsPaginationAndCapsFiles(t *testing.T) {
	hub := newFakeHub(t)
	hub.filePages = [][]map[string]any{
		{{"filename": "a.go", "patch": "p"}, {"filename": "b.go", "patch": "p"}},
		{{"filename": "c.go", "patch": "p"}, {"filename": "d.go", "patch": "p"}},
	}
	set := newSet(t, hub, func(c *github.Config) { c.MaxFiles = 3 })

	out, err := invoke(t, toolNamed(t, set, "github_pr_fetch"), `{"number":7}`)
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

	out, err := invoke(t, toolNamed(t, set, "github_pr_fetch"), `{"number":7}`)
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
	if strings.HasSuffix(f.Patch, "line") || strings.Contains(f.Patch, "line t\n") {
		t.Fatalf("patch cut mid-line: %q", f.Patch)
	}
	if f.Patch != "line one" {
		t.Fatalf("patch = %q, want %q", f.Patch, "line one")
	}
}

// --- auth --------------------------------------------------------------------

// The installation token is minted once and reused while valid, so a review does
// not mint a token per API call.
func TestInstallationTokenIsCached(t *testing.T) {
	hub := newFakeHub(t)
	set := newSet(t, hub, nil)

	for range 3 {
		if _, err := invoke(t, toolNamed(t, set, "github_pr_fetch"), `{"number":7}`); err != nil {
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

	if _, err := invoke(t, toolNamed(t, set, "github_pr_fetch"), `{"number":7}`); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got := hub.tokensMinted.Load(); got != 1 {
		t.Fatalf("tokens minted = %d, want 1", got)
	}

	// The hub issues tokens expiring an hour from the test clock; advance past it.
	clk.Advance(2 * time.Hour)
	if _, err := invoke(t, toolNamed(t, set, "github_pr_fetch"), `{"number":7}`); err != nil {
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
			Owner: "o", Repo: "r",
		}
	}
	cases := map[string]func(*github.Config){
		"no owner":        func(c *github.Config) { c.Owner = "" },
		"no repo":         func(c *github.Config) { c.Repo = "" },
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

	_, err := invoke(t, toolNamed(t, set, "github_pr_fetch"), `{"number":7}`)
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

	out, err := invoke(t, toolNamed(t, set, "github_pr_fetch"), `{"number":7}`)
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

	if _, err := invoke(t, toolNamed(t, set, "github_pr_fetch"), `{"number":7}`); err != nil {
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

	if _, err := invoke(t, toolNamed(t, set, "github_pr_fetch"), `{"number":7}`); !errors.Is(err, github.ErrDiffTooLarge) {
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

	_, err := invoke(t, toolNamed(t, set, "github_submit_review"), `{"number":7,"event":"APPROVE"}`)
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

	_, err := invoke(t, toolNamed(t, set, "github_submit_review"), `{"number":7,"event":"APPROVE"}`)
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

	if _, err := invoke(t, toolNamed(t, set, "github_submit_review"), `{"number":7,"event":"REQUEST_CHANGES"}`); err != nil {
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

	if _, err := invoke(t, toolNamed(t, set, "github_submit_review"), `{"number":7,"event":"APPROVE"}`); err != nil {
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

	if _, err := invoke(t, toolNamed(t, set, "github_submit_review"), `{"number":7,"event":"APPROVE"}`); err != nil {
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

	out, err := invoke(t, toolNamed(t, set, "github_pr_fetch"), `{"number":7}`)
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

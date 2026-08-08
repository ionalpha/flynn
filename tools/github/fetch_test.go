package github_test

// Reading the pull request: the diff, the posted markers, pagination and the file cap.
// A patch too large to send is cut on a line boundary and flagged as cut, because a
// patch silently truncated mid-line would be reviewed as though it were whole. A
// pagination link pointing off the API host is refused rather than followed.

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/tools/github"
)

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

package github_test

// The review budget and diff completeness. A runaway diff is refused before a model
// token is spent on it, counted from the pull request's own totals rather than the
// bytes that happened to be fetched. Completeness drives the approve gate separately
// from size: a diff the reviewer could not see whole cannot be approved, however small,
// while a complete diff can be approved however large.

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/ionalpha/flynn/tools/github"
)

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

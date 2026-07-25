package progress

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/spine"
)

// failingLog is a spine.Log whose Read always errors, so a test can drive the probe's
// read-failure path.
type failingLog struct{}

func (failingLog) Append(context.Context, spine.AppendInput) (spine.Event, error) {
	return spine.Event{}, nil
}

func (failingLog) Read(context.Context, spine.Query) ([]spine.Event, error) {
	return nil, errors.New("spine unavailable")
}
func (failingLog) SaveSnapshot(context.Context, spine.Snapshot) error { return nil }
func (failingLog) LatestSnapshot(context.Context, string, int64) (spine.Snapshot, bool, error) {
	return spine.Snapshot{}, false, nil
}

// TestNewSpineProbeReadsAgainstARealWorkdir exercises the real constructor (its default
// git-HEAD reader over an actual directory), not the test's struct literal: a workdir
// with no .git yields a non-empty fingerprint and a real tool call is read back.
func TestNewSpineProbeReadsAgainstARealWorkdir(t *testing.T) {
	log := spine.NewMemoryLog()
	p := NewSpineProbe(log, t.TempDir()) // real readGitHead, non-git dir
	appendToolCall(t, log, "read", `{"path":"a.txt"}`)

	r := resource.Resource{APIVersion: goal.GroupVersion, Kind: goal.Kind, Name: stream}
	fp, summary, err := p.Progress(context.Background(), r)
	if err != nil {
		t.Fatalf("Progress: %v", err)
	}
	if fp == "" {
		t.Fatal("empty fingerprint from the real probe")
	}
	if !strings.Contains(summary, "read") {
		t.Fatalf("summary did not name the last action: %q", summary)
	}
}

// TestProgressReturnsTransientOnAReadFailure: a spine read that fails is a transient
// error, not a stall — a probe that cannot reach the record for a moment must not read as
// the run having made no progress.
func TestProgressReturnsTransientOnAReadFailure(t *testing.T) {
	p := NewSpineProbe(failingLog{}, t.TempDir())
	r := resource.Resource{APIVersion: goal.GroupVersion, Kind: goal.Kind, Name: stream}
	if _, _, err := p.Progress(context.Background(), r); err == nil {
		t.Fatal("a failed spine read did not surface an error")
	}
}

// TestDescribeActionBranches covers the empty/{} short form and the truncation of long
// arguments, so a stall reason stays a readable line whatever the tool args.
func TestDescribeActionBranches(t *testing.T) {
	if got := describeAction("bash", []byte(`{}`)); got != "bash" {
		t.Fatalf("empty-args form: %q, want %q", got, "bash")
	}
	if got := describeAction("bash", nil); got != "bash" {
		t.Fatalf("nil-args form: %q, want %q", got, "bash")
	}
	long := `{"command":"` + strings.Repeat("x", 200) + `"}`
	got := describeAction("bash", []byte(long))
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("long args were not truncated: %q", got)
	}
	if len(got) > 100 {
		t.Fatalf("truncated summary still too long: %d chars", len(got))
	}
}

// TestProvenCountOnMalformedStatus: an undecodable status contributes zero rather than
// crashing the probe.
func TestProvenCountOnMalformedStatus(t *testing.T) {
	r := resource.Resource{APIVersion: goal.GroupVersion, Kind: goal.Kind, Name: stream, Status: []byte("not json")}
	if n := provenCount(r); n != 0 {
		t.Fatalf("malformed status proven count = %d, want 0", n)
	}
}

// TestResolveGitDirFollowsALinkedWorktree covers the .git-as-file form (a linked
// worktree points elsewhere) and the malformed-file fallback.
func TestResolveGitDirFollowsALinkedWorktree(t *testing.T) {
	// .git is a file "gitdir: <path>" pointing at the real git directory.
	work := t.TempDir()
	realGit := t.TempDir()
	mustWrite(t, filepath.Join(work, ".git"), "gitdir: "+realGit+"\n")
	mustWrite(t, filepath.Join(realGit, "HEAD"), "5555555555555555555555555555555555555555\n")
	if h := readGitHead(work); h != "5555555555555555555555555555555555555555" {
		t.Fatalf("linked-worktree HEAD = %q", h)
	}

	// A .git file that is not the "gitdir:" form resolves to no git dir.
	bad := t.TempDir()
	mustWrite(t, filepath.Join(bad, ".git"), "garbage\n")
	if h := readGitHead(bad); h != "" {
		t.Fatalf("malformed .git file yielded a HEAD: %q", h)
	}
}

// TestReadGitHeadUnresolvableRef: HEAD points at a ref with no loose file and no
// packed-refs entry, so it resolves to "" rather than a stale or wrong value.
func TestReadGitHeadUnresolvableRef(t *testing.T) {
	dir := t.TempDir()
	gitdir := filepath.Join(dir, ".git")
	mustMkdir(t, gitdir)
	mustWrite(t, filepath.Join(gitdir, "HEAD"), "ref: refs/heads/absent\n")
	// A packed-refs that does not contain the ref (only comments and a different ref).
	mustWrite(t, filepath.Join(gitdir, "packed-refs"),
		"# pack-refs with: peeled\n^0000000000000000000000000000000000000000\n9999999999999999999999999999999999999999 refs/heads/other\n")
	if h := readGitHead(dir); h != "" {
		t.Fatalf("unresolvable ref yielded a HEAD: %q", h)
	}
}

// TestReadGitHeadMissingHeadFile: a gitdir with no HEAD file resolves to "".
func TestReadGitHeadMissingHeadFile(t *testing.T) {
	dir := t.TempDir()
	mustMkdir(t, filepath.Join(dir, ".git"))
	if h := readGitHead(dir); h != "" {
		t.Fatalf("missing HEAD yielded %q", h)
	}
}

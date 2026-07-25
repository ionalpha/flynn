package progress

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/spine"
)

const stream = "run-1"

// probe builds a SpineProbe over log with a stubbed git-HEAD reader, so a test isolates
// the action and ledger signals from any real .git. head is the sequence of HEAD values
// returned on successive calls (the last repeats).
func probe(log spine.Log, heads ...string) *SpineProbe {
	i := 0
	return &SpineProbe{
		log:     log,
		workdir: "unused",
		head: func(string) string {
			if len(heads) == 0 {
				return ""
			}
			h := heads[i]
			if i < len(heads)-1 {
				i++
			}
			return h
		},
	}
}

// appendToolCall records a tool.call on the stream in the session wire format the probe
// reads (Type "tool.call", the event body folded under the "event" payload key).
func appendToolCall(t *testing.T, log spine.Log, tool, input string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"kind": "tool.call", "tool": tool, "input": json.RawMessage(input)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(context.Background(), spine.AppendInput{
		Stream:  stream,
		Type:    "tool.call",
		Payload: map[string]any{"event": string(body)},
	}); err != nil {
		t.Fatalf("append tool call: %v", err)
	}
}

func goalResource(t *testing.T, ledger ...goal.LedgerState) resource.Resource {
	t.Helper()
	st := goal.Status{Ledger: ledger}
	enc, err := st.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return resource.Resource{APIVersion: goal.GroupVersion, Kind: goal.Kind, Name: stream, Status: enc}
}

func fingerprint(t *testing.T, p *SpineProbe, r resource.Resource) string {
	t.Helper()
	fp, _, err := p.Progress(context.Background(), r)
	if err != nil {
		t.Fatalf("Progress: %v", err)
	}
	if fp == "" {
		t.Fatal("probe returned an empty fingerprint")
	}
	return fp
}

// TestReReadingTheSameFileIsIdle is the core loop the probe must catch: a step that
// re-runs a tool call it already made adds nothing to the distinct-action set, so the
// fingerprint is unchanged and the reconciler will read the step as idle.
func TestReReadingTheSameFileIsIdle(t *testing.T) {
	log := spine.NewMemoryLog()
	p := probe(log, "") // no git
	r := goalResource(t)

	appendToolCall(t, log, "read", `{"path":"a.txt"}`)
	before := fingerprint(t, p, r)

	appendToolCall(t, log, "read", `{"path":"a.txt"}`) // the same call again
	after := fingerprint(t, p, r)

	if before != after {
		t.Fatalf("re-reading the same file changed the fingerprint: %q -> %q", before, after)
	}
}

// TestANewActionIsProgress: a tool call the run has not made before (a different file, or
// different content) grows the action set and changes the fingerprint.
func TestANewActionIsProgress(t *testing.T) {
	log := spine.NewMemoryLog()
	p := probe(log, "")
	r := goalResource(t)

	appendToolCall(t, log, "read", `{"path":"a.txt"}`)
	before := fingerprint(t, p, r)

	appendToolCall(t, log, "read", `{"path":"b.txt"}`) // a new file
	after := fingerprint(t, p, r)

	if before == after {
		t.Fatalf("a new action did not change the fingerprint: %q", before)
	}
}

// TestCommittingCountsAsProgress is the trap the reference circuit breaker fell into:
// a step that commits leaves a clean tree, but HEAD moves. The probe keys on HEAD, so
// with the action set unchanged, a moved HEAD is still progress.
func TestCommittingCountsAsProgress(t *testing.T) {
	log := spine.NewMemoryLog()
	p := probe(log, "commitA", "commitB") // HEAD moves between the two reads
	r := goalResource(t)

	appendToolCall(t, log, "bash", `{"command":"git commit -am work"}`)
	before := fingerprint(t, p, r)
	after := fingerprint(t, p, r) // same events, but HEAD advanced

	if before == after {
		t.Fatalf("a commit (moved HEAD) did not read as progress: %q", before)
	}
}

// TestNonGitWorkspaceDoesNotDisableDetection is the other trap: a non-git root must not
// switch detection off. With no HEAD signal at all, the action set still drives the
// fingerprint, so idle and progress are still distinguishable.
func TestNonGitWorkspaceDoesNotDisableDetection(t *testing.T) {
	log := spine.NewMemoryLog()
	p := probe(log, "") // never a repo
	r := goalResource(t)

	appendToolCall(t, log, "read", `{"path":"a.txt"}`)
	idleBefore := fingerprint(t, p, r)
	appendToolCall(t, log, "read", `{"path":"a.txt"}`) // repeat: idle
	idleAfter := fingerprint(t, p, r)
	if idleBefore != idleAfter {
		t.Fatal("without git, a repeated action was not detected as idle")
	}

	appendToolCall(t, log, "write", `{"path":"c.txt","content":"x"}`) // real work
	if progressed := fingerprint(t, p, r); progressed == idleAfter {
		t.Fatal("without git, real work was not detected as progress")
	}
}

// TestProvenLedgerItemIsProgress: advancing an item through the evidence gate is progress
// even on a step that made no new tool call.
func TestProvenLedgerItemIsProgress(t *testing.T) {
	log := spine.NewMemoryLog()
	p := probe(log, "")

	none := fingerprint(t, p, goalResource(t, goal.LedgerState{ID: "x"}))
	proven := fingerprint(t, p, goalResource(t, goal.LedgerState{ID: "x", Proven: true}))

	if none == proven {
		t.Fatalf("a newly proven ledger item did not read as progress: %q", none)
	}
}

// TestFingerprintNonEmptyWithNoActivity: the probe honors its contract of a non-empty
// fingerprint even when nothing has happened, so two do-nothing steps compare equal and
// the streak can advance rather than resetting forever.
func TestFingerprintNonEmptyWithNoActivity(t *testing.T) {
	log := spine.NewMemoryLog()
	p := probe(log, "")
	r := goalResource(t)

	a := fingerprint(t, p, r)
	b := fingerprint(t, p, r)
	if a != b {
		t.Fatalf("two do-nothing reads differ: %q vs %q", a, b)
	}
}

// TestReadGitHeadReadsALooseRef exercises the real filesystem reader against a hand-built
// .git: a loose branch ref is followed from HEAD, and a moved ref reads as a new value; a
// directory with no .git yields "".
func TestReadGitHeadReadsALooseRef(t *testing.T) {
	dir := t.TempDir()

	// Not a repo yet.
	if h := readGitHead(dir); h != "" {
		t.Fatalf("non-git dir returned a HEAD: %q", h)
	}

	// Build a minimal .git: HEAD -> refs/heads/main -> a commit hash.
	gitdir := filepath.Join(dir, ".git")
	mustMkdir(t, filepath.Join(gitdir, "refs", "heads"))
	mustWrite(t, filepath.Join(gitdir, "HEAD"), "ref: refs/heads/main\n")
	mustWrite(t, filepath.Join(gitdir, "refs", "heads", "main"), "1111111111111111111111111111111111111111\n")

	if h := readGitHead(dir); h != "1111111111111111111111111111111111111111" {
		t.Fatalf("loose ref HEAD = %q", h)
	}

	// A commit moves the ref; the reader reflects it.
	mustWrite(t, filepath.Join(gitdir, "refs", "heads", "main"), "2222222222222222222222222222222222222222\n")
	if h := readGitHead(dir); h != "2222222222222222222222222222222222222222" {
		t.Fatalf("moved ref HEAD = %q", h)
	}
}

// TestReadGitHeadDetachedAndPacked covers the detached-HEAD form and the packed-refs
// fallback, so the reader does not silently return "" (which would drop the git signal)
// for a repo in either state.
func TestReadGitHeadDetachedAndPacked(t *testing.T) {
	// Detached HEAD: the commit hash sits directly in HEAD.
	det := t.TempDir()
	mustMkdir(t, filepath.Join(det, ".git"))
	mustWrite(t, filepath.Join(det, ".git", "HEAD"), "3333333333333333333333333333333333333333\n")
	if h := readGitHead(det); h != "3333333333333333333333333333333333333333" {
		t.Fatalf("detached HEAD = %q", h)
	}

	// Packed refs: HEAD points at a ref with no loose file, resolved from packed-refs.
	packed := t.TempDir()
	gitdir := filepath.Join(packed, ".git")
	mustMkdir(t, gitdir)
	mustWrite(t, filepath.Join(gitdir, "HEAD"), "ref: refs/heads/main\n")
	mustWrite(t, filepath.Join(gitdir, "packed-refs"),
		"# pack-refs with: peeled fully-peeled sorted\n4444444444444444444444444444444444444444 refs/heads/main\n")
	if h := readGitHead(packed); h != "4444444444444444444444444444444444444444" {
		t.Fatalf("packed ref HEAD = %q", h)
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o750); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

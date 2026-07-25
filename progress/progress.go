// Package progress implements goal.ProgressProbe over a run's durable record: it reads
// the run's own event stream from the spine and the git HEAD at its working directory,
// and derives a fingerprint of the substantive work done so far. The goal reconciler
// compares successive fingerprints to tell a step that got somewhere from one that spun,
// and stops a run that has stopped getting anywhere.
//
// It lives in its own package rather than in mission because it reads the session
// projection of the spine, and session already depends on mission; importing session
// from mission would be a cycle. Here it depends on goal (the port), session (the reader)
// and spine (the log), and nothing depends on it but the composition that wires it.
package progress

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/session"
	"github.com/ionalpha/flynn/spine"
)

// SpineProbe is the goal.ProgressProbe reading a run's recorded activity. It reads three
// signals, and deliberately not the working tree:
//
//   - The set of distinct tool actions (tool name + arguments) the run has taken. A step
//     that re-reads the same file makes a tool call whose (tool, args) is already in the
//     set, so the set does not grow and the step reads as idle — the exact loop
//     no-progress detection exists to catch. A step that reads a new file, writes new
//     content, or runs a new command grows the set and reads as progress.
//   - The git HEAD at the workdir, read from the filesystem. A committing step leaves a
//     clean tree but moves HEAD, so keying on HEAD (not on tree dirtiness) counts the
//     commit as progress — the failure that sank the reference circuit breaker, which
//     read a committed, clean tree as a dead loop. A workspace that is not a git repo
//     simply yields no HEAD; detection is not disabled, it runs on the other signals,
//     unlike the rev-parse gate that switched the whole check off in a non-git root.
//   - The number of proven ledger items, so an item advanced through the evidence gate
//     (F2) counts as progress even on a step that touched no file.
//
// The fingerprint is non-empty even when nothing has happened, as goal.ProgressProbe
// requires: two do-nothing steps then compare equal and the idle streak advances.
type SpineProbe struct {
	log     spine.Log
	workdir string
	// head reads the current commit at a directory, or "" when it is not a repo. It is a
	// field so a test supplies a deterministic reader instead of touching a real .git.
	head func(dir string) string
}

var _ goal.ProgressProbe = (*SpineProbe)(nil)

// NewSpineProbe builds a progress probe reading run streams from log and the git HEAD at
// workdir. The stream to read is the goal's own id, taken from the resource on each call,
// so one probe serves a fan-out's children as well as its root.
func NewSpineProbe(log spine.Log, workdir string) *SpineProbe {
	return &SpineProbe{log: log, workdir: workdir, head: readGitHead}
}

// Progress reads the run's recorded events and returns a fingerprint of the substantive
// work observed and a short description of the last thing the run did. A failed read is
// transient: a probe that cannot reach the record for a moment must not be read as the
// run having made no progress, which would stall a healthy goal.
func (p *SpineProbe) Progress(ctx context.Context, r resource.Resource) (string, string, error) {
	events, err := session.History(ctx, p.log, r.Name)
	if err != nil {
		return "", "", fault.Wrap(fault.Transient, "progress_history_read", err)
	}

	actions := make(map[string]struct{})
	var lastActivity string
	for _, e := range events {
		if e.Kind != session.KindToolCall {
			continue
		}
		actions[actionKey(e.Tool, e.Input)] = struct{}{}
		lastActivity = describeAction(e.Tool, e.Input)
	}

	head := p.head(p.workdir)
	proven := provenCount(r)

	fingerprint := fmt.Sprintf("actions=%d|head=%s|proven=%d", len(actions), head, proven)
	if lastActivity == "" {
		lastActivity = "no tool activity recorded"
	}
	return fingerprint, lastActivity, nil
}

// actionKey is the identity of one tool action: its name and arguments, hashed so a
// repeated call collides with its earlier self (leaving the action set unchanged) while a
// call with any different argument is a new action. Hashing keeps the key bounded whatever
// the argument size.
func actionKey(tool string, input []byte) string {
	sum := sha256.Sum256(append([]byte(tool+"\x00"), input...))
	return hex.EncodeToString(sum[:])
}

// describeAction renders a short summary of a tool call for the stall reason: the tool
// name and a clipped view of its arguments, so a no-progress stall can say "last doing:
// read {\"path\":\"config.yaml\"}" rather than only a count.
func describeAction(tool string, input []byte) string {
	args := strings.TrimSpace(string(input))
	const maxArgs = 80
	if len(args) > maxArgs {
		args = args[:maxArgs] + "…"
	}
	if args == "" || args == "{}" {
		return tool
	}
	return tool + " " + args
}

// provenCount is how many of the goal's ledger items are proven. An item advancing to
// proven is progress on a step that may have touched nothing else, so it is one of the
// probe's signals. A goal with no ledger contributes zero, stable across steps and so
// never by itself reading as progress or as idle.
func provenCount(r resource.Resource) int {
	status, err := goal.DecodeStatus(r)
	if err != nil {
		return 0
	}
	n := 0
	for _, st := range status.Ledger {
		if st.Proven {
			n++
		}
	}
	return n
}

// readGitHead returns the commit HEAD currently points at for the repository at dir, or
// "" when dir is not a git repository (or HEAD cannot be resolved). It reads the
// filesystem rather than shelling out to git: it resolves the gitdir (handling a
// worktree, whose .git is a file pointing elsewhere), reads HEAD, follows a symbolic ref
// to its loose ref file, and falls back to packed-refs. A new commit always rewrites the
// loose ref, so this reflects the latest commit for progress detection without spawning a
// process.
func readGitHead(dir string) string {
	gitdir := resolveGitDir(dir)
	if gitdir == "" {
		return ""
	}
	// #nosec G304 -- gitdir is resolved from the run's own working directory; git metadata
	// is read read-only to derive a progress signal, not to include user-named content.
	head, err := os.ReadFile(filepath.Join(gitdir, "HEAD"))
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(head))
	ref, ok := strings.CutPrefix(line, "ref:")
	if !ok {
		// Detached HEAD: the file holds the commit hash directly.
		return line
	}
	ref = strings.TrimSpace(ref)
	// #nosec G304 G703 -- the ref path is composed from the repo's own HEAD, under its
	// gitdir; it is read read-only for a progress signal, never used to admit content.
	if hash, err := os.ReadFile(filepath.Join(gitdir, filepath.FromSlash(ref))); err == nil {
		return strings.TrimSpace(string(hash))
	}
	return packedRef(gitdir, ref)
}

// resolveGitDir returns the git directory for a working tree at dir: dir/.git when it is a
// directory, or the path a dir/.git file points at (the linked-worktree form). It returns
// "" when neither exists, which is how a non-git workspace yields no HEAD signal.
func resolveGitDir(dir string) string {
	dotgit := filepath.Join(dir, ".git")
	info, err := os.Stat(dotgit)
	if err != nil {
		return ""
	}
	if info.IsDir() {
		return dotgit
	}
	// #nosec G304 -- dotgit is <workdir>/.git for the run's own working directory.
	body, err := os.ReadFile(dotgit)
	if err != nil {
		return ""
	}
	if p, ok := strings.CutPrefix(strings.TrimSpace(string(body)), "gitdir:"); ok {
		return strings.TrimSpace(p)
	}
	return ""
}

// packedRef looks ref up in a repository's packed-refs file, the fallback when a ref has
// no loose file (after git gc packs them). It returns the commit hash, or "" if the ref
// is not packed either.
func packedRef(gitdir, ref string) string {
	// #nosec G304 -- packed-refs is read from the repo's own gitdir, read-only.
	f, err := os.Open(filepath.Join(gitdir, "packed-refs"))
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "^") {
			continue
		}
		hash, name, ok := strings.Cut(line, " ")
		if ok && name == ref {
			return hash
		}
	}
	return ""
}

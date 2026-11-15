// Package progress implements goal.ProgressProbe: it fingerprints the work a run has
// done from its event stream and the git HEAD at its working directory, so the goal
// reconciler can stop a run whose fingerprint stops changing.
//
// It is its own package because it reads the session projection, and session imports
// mission.
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

// SpineProbe fingerprints three signals:
//
//   - the set of distinct tool actions (name and arguments), so a step that re-reads the
//     same file adds nothing and reads as idle;
//   - the git HEAD at the workdir, because a committing step leaves a clean tree but
//     moves HEAD (a non-git workspace contributes no HEAD);
//   - the number of proven ledger items.
//
// The working tree is not read: dirtiness does not tell a commit from no work. The
// fingerprint is non-empty when nothing has happened, so two idle steps compare equal.
type SpineProbe struct {
	log     spine.Log
	workdir string
	// head returns the commit at a directory, or "" outside a repo. Tests replace it.
	head func(dir string) string
}

var _ goal.ProgressProbe = (*SpineProbe)(nil)

// NewSpineProbe builds a probe over log and the repository at workdir. The stream is the
// goal's id, taken from the resource on each call, so one probe serves a whole fan-out.
func NewSpineProbe(log spine.Log, workdir string) *SpineProbe {
	return &SpineProbe{log: log, workdir: workdir, head: readGitHead}
}

// Progress returns the fingerprint and a description of the last action. A failed read
// is transient, so an unreachable record never stalls a healthy goal as idle.
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

// actionKey hashes a tool name and its arguments, so a repeated call collides with its
// earlier self and the key stays bounded whatever the argument size.
func actionKey(tool string, input []byte) string {
	sum := sha256.Sum256(append([]byte(tool+"\x00"), input...))
	return hex.EncodeToString(sum[:])
}

// describeAction renders a tool call for the stall reason, arguments clipped to 80 bytes.
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

// provenCount counts the goal's proven ledger items. A goal with no ledger contributes a
// constant zero.
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

// readGitHead returns the commit HEAD names at dir, or "" when it cannot be resolved.
// It reads files so no git process is spawned; a new commit always rewrites the loose
// ref, so loose refs win over packed-refs.
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

// resolveGitDir returns dir/.git, or the path a .git file points at in a linked
// worktree, or "" when neither exists.
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

// packedRef looks ref up in packed-refs, where git gc moves loose refs.
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

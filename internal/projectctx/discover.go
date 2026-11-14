// Package projectctx loads a repository's instruction files (AGENTS.md,
// AGENTS.override.md, CLAUDE.md) into a session's prompt as untrusted context, and
// records the decision to do so.
//
// The files are written by whoever controls the repository, which is not the
// operator of the session: a cloned repository can carry any text in them. Loading
// one is therefore a trust decision, not a convenience, and the package keeps four
// properties:
//
//   - Nothing loads without a decision. [Decide] resolves a flag, then a remembered
//     choice for this exact content, then an interactive question; with no one to ask
//     it skips, so an unattended run fails closed.
//   - The decision is visible and recorded. [Decision.Summary] is the line a user
//     sees, and [Decision.Append] writes the same facts, with a hash of every file, to
//     the run's spine stream, so the sealed record shows which instructions the model
//     was given.
//   - The text is fenced as data. [Decision.Prompt] wraps each file in a tag whose
//     closing form carries a hash of the file, which a file cannot predict about
//     itself, so it cannot close its own fence and continue as the system prompt.
//   - The session is tainted. [Decision.Taint] returns provenance reasons in the
//     memory guard's untrusted external scheme, which the caller feeds into the
//     same taint scope tool output uses, so nothing learned from a session that read
//     these files is pushed at wake.
//
// The package reads files and writes one JSON file for remembered choices; it knows
// nothing about the REPL, the terminal or any host.
package projectctx

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/ionalpha/flynn/memory/guard"
)

// The instruction file names, in the order they load within one directory.
const (
	// FileAgents is the shared instruction file several agent tools read.
	FileAgents = "AGENTS.md"
	// FileOverride replaces [FileAgents] in the directory that holds it, so a person can
	// keep a private variant beside a committed file without editing the committed one.
	FileOverride = "AGENTS.override.md"
	// FileClaude is the instruction file Claude Code reads.
	FileClaude = "CLAUDE.md"
)

// Size limits. They bound what a hostile repository can put in the prompt, and the
// cost of a large file nobody meant to send.
const (
	// MaxFileBytes is the most of one file that is read; the rest is dropped and the
	// file is marked [File.Truncated].
	MaxFileBytes = 32 << 10
	// MaxTotalBytes is the most all files together may add. Files nearer the working
	// directory are read first, so the budget goes to the most specific instructions.
	MaxTotalBytes = 96 << 10
)

// File is one instruction file as it will be shown to the model.
type File struct {
	// Path is slash-separated and relative to [Set.Root].
	Path string
	// SHA256 is the hex hash of the bytes read, after the size cap. It covers exactly
	// what the model is shown, so a remembered approval lapses when that text changes
	// and does not when only an unread tail does.
	SHA256 string
	// Bytes is how many bytes were read.
	Bytes int
	// Truncated reports that the file was longer than the cap allowed.
	Truncated bool
	// Findings names the kinds of hidden-text signals the memory guard's screen found
	// in the raw bytes (invisible characters, bidirectional overrides, tag-character
	// payloads, override phrasing). They are shown to the person deciding; the
	// characters themselves are stripped from [File.Content] whether or not anyone
	// reads the warning.
	Findings []string
	// Content is the text shown to the model, with control and format characters
	// removed and invalid UTF-8 replaced.
	Content string
}

// Skip is a candidate that was found and not loaded, with why.
type Skip struct {
	Path   string
	Reason string
}

// Set is what [Discover] found for one working directory.
type Set struct {
	// Root is the directory the paths are relative to: the nearest enclosing git
	// checkout, or the working directory itself when there is none.
	Root string
	// Files are in prompt order: outermost directory first, so the file nearest the
	// working directory comes last and is the more specific statement.
	Files []File
	// Skipped lists candidates left out, so a person is not left wondering why a file
	// they have is absent.
	Skipped []Skip
}

// Empty reports that there is nothing to decide about: no file was loadable.
func (s Set) Empty() bool { return len(s.Files) == 0 }

// Digest is a hash over the path and content hash of every file, in order. It is the
// key a remembered choice is stored against.
func (s Set) Digest() string {
	h := sha256.New()
	for _, f := range s.Files {
		_, _ = io.WriteString(h, f.Path+"\x00"+f.SHA256+"\n")
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Paths lists the loaded files' paths, in prompt order.
func (s Set) Paths() []string {
	out := make([]string, len(s.Files))
	for i, f := range s.Files {
		out[i] = f.Path
	}
	return out
}

// Discover finds the instruction files that apply to workdir. It looks in the
// working directory and each parent up to the enclosing git checkout; with no
// checkout it looks in the working directory alone, because walking further would load
// a file from a directory the user never chose (a home directory, a shared parent).
//
// A candidate is skipped, not loaded, when it is a symlink that leaves the root (a
// repository could point CLAUDE.md at a file in the user's home directory), is not a
// regular file, or contains a NUL byte. Within one directory [FileOverride] replaces
// [FileAgents], and a [FileClaude] with the same content as the file before it is
// dropped, since the two are often one file under two names.
func Discover(workdir string) (Set, error) {
	abs, err := filepath.Abs(workdir)
	if err != nil {
		return Set{}, err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return Set{}, err
	}
	root := checkoutRoot(resolved)
	set := Set{Root: root}

	var cands []candidate
	for _, dir := range dirsFromRoot(root, resolved) {
		cands = append(cands, directoryCandidates(root, dir, &set)...)
	}

	// Later candidates are nearer the working directory and more specific, so they are
	// read first and the total budget runs out on the outermost ones.
	budget := MaxTotalBytes
	read := make([]File, len(cands))
	ok := make([]bool, len(cands))
	for i := len(cands) - 1; i >= 0; i-- {
		c := cands[i]
		if budget <= 0 {
			set.Skipped = append(set.Skipped, Skip{c.rel, "over the total size limit"})
			continue
		}
		f, reason := readCandidate(c, min(MaxFileBytes, budget))
		if reason != "" {
			set.Skipped = append(set.Skipped, Skip{c.rel, reason})
			continue
		}
		budget -= f.Bytes
		read[i], ok[i] = f, true
	}

	seen := map[string]string{} // directory -> content hash of the file loaded first
	for i, c := range cands {
		if !ok[i] {
			continue
		}
		dir := filepath.ToSlash(filepath.Dir(c.rel))
		if prev, dup := seen[dir]; dup && prev == read[i].SHA256 {
			set.Skipped = append(set.Skipped, Skip{c.rel, "same content as the file loaded before it"})
			continue
		}
		if _, first := seen[dir]; !first {
			seen[dir] = read[i].SHA256
		}
		set.Files = append(set.Files, read[i])
	}
	return set, nil
}

type candidate struct {
	rel  string // slash-separated, relative to the root
	path string // symlink-resolved absolute path
}

// directoryCandidates returns the files in dir that may load, recording the ones
// that may not in set.Skipped.
func directoryCandidates(root, dir string, set *Set) []candidate {
	names := []string{FileAgents, FileClaude}
	if _, err := os.Lstat(filepath.Join(dir, FileOverride)); err == nil {
		names = []string{FileOverride, FileClaude}
		rel, _ := filepath.Rel(root, filepath.Join(dir, FileAgents))
		if _, err := os.Lstat(filepath.Join(dir, FileAgents)); err == nil {
			set.Skipped = append(set.Skipped, Skip{filepath.ToSlash(rel), "replaced by " + FileOverride})
		}
	}
	var out []candidate
	for _, name := range names {
		p := filepath.Join(dir, name)
		if _, err := os.Lstat(p); err != nil {
			continue
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		resolved, err := filepath.EvalSymlinks(p)
		if err != nil {
			set.Skipped = append(set.Skipped, Skip{rel, "unreadable link"})
			continue
		}
		if !within(root, resolved) {
			set.Skipped = append(set.Skipped, Skip{rel, "resolves outside the repository"})
			continue
		}
		if st, err := os.Stat(resolved); err != nil || !st.Mode().IsRegular() {
			set.Skipped = append(set.Skipped, Skip{rel, "not a regular file"})
			continue
		}
		out = append(out, candidate{rel: rel, path: resolved})
	}
	return out
}

// readCandidate reads up to limit bytes of c. A non-empty reason means the file is
// not loaded.
func readCandidate(c candidate, limit int) (File, string) {
	f, err := os.Open(c.path)
	if err != nil {
		return File{}, "unreadable"
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if err != nil {
		return File{}, "unreadable"
	}
	truncated := len(raw) > limit
	if truncated {
		raw = raw[:limit]
	}
	if len(raw) == 0 {
		return File{}, "empty"
	}
	if strings.IndexByte(string(raw), 0) >= 0 {
		return File{}, "binary content"
	}
	sum := sha256.Sum256(raw)
	var findings []string
	for _, fd := range guard.Screen(string(raw)) {
		findings = append(findings, string(fd.Kind))
	}
	return File{
		Path:      c.rel,
		SHA256:    hex.EncodeToString(sum[:]),
		Bytes:     len(raw),
		Truncated: truncated,
		Findings:  findings,
		Content:   sanitize(string(raw)),
	}, ""
}

// sanitize replaces invalid UTF-8 and drops control and format characters other than
// tab, newline and carriage return. Format characters are the carrier for text a
// reviewer cannot see (zero-width, bidirectional and tag characters), so removing
// the class is simpler and safer than listing them; it also removes the joiner in a
// multi-part emoji, which an instruction file can live without.
func sanitize(s string) string {
	s = strings.ToValidUTF8(s, "�")
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\t' || r == '\n' || r == '\r':
			return r
		case unicode.IsControl(r), unicode.Is(unicode.Cf, r):
			return -1
		}
		return r
	}, s)
}

// checkoutRoot returns the nearest directory at or above dir that holds a .git entry
// (a directory, or the file a worktree has), or dir itself when there is none.
func checkoutRoot(dir string) string {
	for d := dir; ; {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			return d
		}
		parent := filepath.Dir(d)
		if parent == d {
			return dir
		}
		d = parent
	}
}

// dirsFromRoot lists the directories from root down to dir, inclusive.
func dirsFromRoot(root, dir string) []string {
	var rev []string
	for d := dir; ; d = filepath.Dir(d) {
		rev = append(rev, d)
		if d == root || filepath.Dir(d) == d {
			break
		}
	}
	out := make([]string, len(rev))
	for i, d := range rev {
		out[len(rev)-1-i] = d
	}
	return out
}

// within reports whether path is root or below it.
func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

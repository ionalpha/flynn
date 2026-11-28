package pathglob

import (
	"path"
	"strings"
)

// Ignore holds the rules of one ignore file, or of several that share a
// directory, in .gitignore syntax. The last matching rule wins, as in git.
type Ignore struct {
	rules []ignoreRule
}

type ignoreRule struct {
	segs     []string
	negate   bool // a ! rule re-includes a path an earlier rule excluded
	dirOnly  bool // a trailing / restricts the rule to directories
	anchored bool // a leading or interior / ties the pattern to the file's directory
}

// ParseIgnore builds an Ignore from file contents. A line git would reject is
// skipped.
func ParseIgnore(content []byte) *Ignore {
	ig := &Ignore{}
	ig.Add(content)
	return ig
}

// Add appends another file's rules, which take precedence over those already
// held.
func (ig *Ignore) Add(content []byte) {
	for line := range strings.SplitSeq(strings.TrimPrefix(string(content), "\ufeff"), "\n") {
		line = trimTrailingSpace(strings.TrimRight(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		var r ignoreRule
		line, r.negate = strings.CutPrefix(line, "!")
		line, r.dirOnly = strings.CutSuffix(line, "/")
		r.anchored = strings.Contains(line, "/")
		line = strings.TrimPrefix(line, "/")
		if line == "" {
			continue
		}
		r.segs = split(classNegation(line))
		// A trailing ** covers what is inside a directory, never the directory
		// itself, so it needs at least one segment.
		if n := len(r.segs); n > 1 && r.segs[n-1] == "**" {
			r.segs = append(r.segs, "*")
		}
		ig.rules = append(ig.rules, r)
	}
}

// trimTrailingSpace drops trailing blanks unless a backslash escapes them.
func trimTrailingSpace(s string) string {
	t := strings.TrimRight(s, " \t")
	if len(t) < len(s) && strings.HasSuffix(t, `\`) {
		return s[:len(t)+1]
	}
	return t
}

// Empty reports whether there are no rules.
func (ig *Ignore) Empty() bool { return len(ig.rules) == 0 }

// Decide judges a slash-separated path relative to the ignore file's directory.
// matched is false when no rule speaks about it, which leaves the verdict to a
// file higher up.
func (ig *Ignore) Decide(rel string, isDir bool) (ignored, matched bool) {
	segs := split(rel)
	if len(segs) == 0 {
		return false, false
	}
	base := segs[len(segs)-1]
	for i := len(ig.rules) - 1; i >= 0; i-- {
		r := ig.rules[i]
		if r.dirOnly && !isDir {
			continue
		}
		var hit bool
		if r.anchored {
			hit = matchSegments(r.segs, segs)
		} else {
			ok, err := path.Match(r.segs[0], base)
			hit = err == nil && ok
		}
		if hit {
			return !r.negate, true
		}
	}
	return false, false
}

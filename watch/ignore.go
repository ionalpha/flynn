package watch

import (
	"path"
	"strings"
)

// Ignore matches repo-relative paths against a set of .gitignore rules so the watch
// walk skips the files git would. It covers the common cases: blank and # comment
// lines, ! negation, a trailing / for directory-only rules, a leading / to anchor to
// the root, * globs within a path segment, and ** to span segments. Last matching
// rule wins, matching git's precedence. It is not the full gitignore grammar (no
// character classes, no rooted-vs-nested subtleties beyond anchoring); it errs
// toward matching the everyday patterns a working tree actually carries.
type Ignore struct {
	rules []ignoreRule
}

type ignoreRule struct {
	glob     string // pattern with any leading / and trailing / removed, slash-separated
	negate   bool   // a ! rule re-includes a path an earlier rule excluded
	dirOnly  bool   // a trailing / restricts the rule to directories
	anchored bool   // a leading / or an interior / anchors the pattern to the root
}

// ParseIgnore builds an Ignore from .gitignore file contents. Paths are always
// matched with forward slashes regardless of the host separator.
func ParseIgnore(content []byte) *Ignore {
	ig := &Ignore{}
	for _, line := range strings.Split(string(content), "\n") {
		line = strings.TrimRight(line, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		r := ignoreRule{glob: trimmed}
		if strings.HasPrefix(r.glob, "!") {
			r.negate = true
			r.glob = r.glob[1:]
		}
		if strings.HasSuffix(r.glob, "/") {
			r.dirOnly = true
			r.glob = strings.TrimSuffix(r.glob, "/")
		}
		// A slash anywhere but a trailing one anchors the pattern to the ignore root.
		r.anchored = strings.Contains(r.glob, "/")
		r.glob = strings.TrimPrefix(r.glob, "/")
		if r.glob == "" {
			continue
		}
		ig.rules = append(ig.rules, r)
	}
	return ig
}

// Match reports whether the repo-relative path (slash-separated) is ignored. isDir
// lets a directory-only rule apply only to directories; a walk prunes an ignored
// directory, so its descendants are never visited.
func (ig *Ignore) Match(rel string, isDir bool) bool {
	rel = strings.Trim(path.Clean(strings.ReplaceAll(rel, "\\", "/")), "/")
	if rel == "" || rel == "." {
		return false
	}
	base := rel
	if i := strings.LastIndex(rel, "/"); i >= 0 {
		base = rel[i+1:]
	}
	matched := false
	for _, r := range ig.rules {
		if r.dirOnly && !isDir {
			continue
		}
		var m bool
		if r.anchored {
			m = matchGlob(r.glob, rel)
		} else {
			// An unanchored pattern matches at any depth: against the basename, or as
			// though it were prefixed with **/ against the whole path.
			m = matchGlob(r.glob, base) || matchGlob("**/"+r.glob, rel)
		}
		if m {
			matched = !r.negate
		}
	}
	return matched
}

// matchGlob matches a slash-separated glob against a slash-separated path, where **
// spans zero or more segments and * / ? match within one segment (path.Match).
func matchGlob(pattern, name string) bool {
	return matchParts(splitSlash(pattern), splitSlash(name))
}

func splitSlash(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "/")
}

func matchParts(pat, name []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			if len(pat) == 1 {
				return true // trailing ** matches every remaining segment
			}
			for i := 0; i <= len(name); i++ {
				if matchParts(pat[1:], name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		if ok, err := path.Match(pat[0], name[0]); err != nil || !ok {
			return false
		}
		pat, name = pat[1:], name[1:]
	}
	return len(name) == 0
}

package watch

import (
	"path"
	"strings"
)

// Ignore matches paths against .gitignore rules: # comments, ! negation, trailing
// / for directories, leading / to anchor, * within a segment and ** across them.
// The last matching rule wins, as in git. Character classes and nested ignore
// files are not supported.
type Ignore struct {
	rules []ignoreRule
}

type ignoreRule struct {
	glob     string // pattern with any leading / and trailing / removed, slash-separated
	negate   bool   // a ! rule re-includes a path an earlier rule excluded
	dirOnly  bool   // a trailing / restricts the rule to directories
	anchored bool   // a leading / or an interior / anchors the pattern to the root
}

// ParseIgnore builds an Ignore from .gitignore contents. Paths match with forward
// slashes on every host.
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

// Match reports whether the repo-relative path is ignored. isDir lets a
// directory-only rule apply. A walk prunes an ignored directory, so a negation
// cannot re-include a file inside it.
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
			// An unanchored pattern matches at any depth.
			m = matchGlob(r.glob, base) || matchGlob("**/"+r.glob, rel)
		}
		if m {
			matched = !r.negate
		}
	}
	return matched
}

func matchGlob(pattern, name string) bool {
	return matchParts(splitSlash(pattern), splitSlash(name))
}

func splitSlash(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "/")
}

// matchParts is a dynamic program because recursive matching backtracks
// exponentially on several ** segments, and the ignore file comes from the watched
// tree, so a hostile pattern must stay cheap. prev[j] records whether the pattern
// so far matches the first j name segments.
func matchParts(pat, name []string) bool {
	prev := make([]bool, len(name)+1)
	cur := make([]bool, len(name)+1)
	prev[0] = true
	for _, p := range pat {
		if p == "**" {
			// ** matches zero segments (prev[j]) or one more (cur[j-1]).
			cur[0] = prev[0]
			for j := 1; j <= len(name); j++ {
				cur[j] = prev[j] || cur[j-1]
			}
		} else {
			cur[0] = false
			for j := 1; j <= len(name); j++ {
				cur[j] = false
				if prev[j-1] {
					ok, err := path.Match(p, name[j-1])
					cur[j] = err == nil && ok
				}
			}
		}
		prev, cur = cur, prev
	}
	return prev[len(name)]
}

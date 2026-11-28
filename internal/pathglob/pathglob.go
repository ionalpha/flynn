// Package pathglob matches slash-separated paths against glob patterns with the
// doublestar syntax and against .gitignore rules.
//
// Within one segment *, ? and [...] mean what they do in [path.Match], with [!...]
// also negating a class. A segment that is exactly ** matches any number of
// segments, including none. {a,b} matches either alternative and may nest.
package pathglob

import (
	"errors"
	"fmt"
	"path"
	"strings"
)

// maxAlternatives bounds brace expansion, which grows multiplicatively with the
// number of groups in a pattern.
const maxAlternatives = 1024

var (
	errUnclosedBrace = errors.New("unclosed brace in glob pattern")
	errTooManyBraces = fmt.Errorf("glob pattern expands to over %d alternatives", maxAlternatives)
)

// Pattern is a compiled glob.
type Pattern struct {
	alts [][]string
}

// Compile expands braces and checks every segment's syntax.
func Compile(pattern string) (*Pattern, error) {
	expanded, err := expand(pattern)
	if err != nil {
		return nil, err
	}
	p := &Pattern{alts: make([][]string, 0, len(expanded))}
	for _, alt := range expanded {
		var segs []string
		for _, s := range split(classNegation(alt)) {
			if s == "" || s == "." {
				continue
			}
			if _, err := path.Match(s, ""); err != nil {
				return nil, fmt.Errorf("bad glob pattern %q: %w", pattern, err)
			}
			segs = append(segs, s)
		}
		p.alts = append(p.alts, segs)
	}
	return p, nil
}

// Alternatives returns each brace-free alternative as its segments.
func (p *Pattern) Alternatives() [][]string { return p.alts }

// Match reports whether the whole of name matches.
func (p *Pattern) Match(name string) bool {
	segs := split(name)
	for _, alt := range p.alts {
		if matchSegments(alt, segs) {
			return true
		}
	}
	return false
}

// MayContain reports whether some path below dir could match, so a walk can skip
// a directory that cannot contribute.
func (p *Pattern) MayContain(dir string) bool {
	segs := split(dir)
	for _, alt := range p.alts {
		if mayContain(alt, segs) {
			return true
		}
	}
	return false
}

func mayContain(pat, dir []string) bool {
	for i, d := range dir {
		if i >= len(pat) {
			return false
		}
		if pat[i] == "**" {
			return true
		}
		if ok, err := path.Match(pat[i], d); err != nil || !ok {
			return false
		}
	}
	return len(pat) > len(dir)
}

// Literal reports whether a segment names exactly one entry.
func Literal(segment string) bool { return !strings.ContainsAny(segment, `*?[\`) }

func split(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "/")
}

// matchSegments is a dynamic program because recursive matching backtracks
// exponentially on several ** segments, and a pattern can come from a file in the
// searched tree, so a hostile one must stay cheap. prev[j] records whether the
// pattern so far matches the first j name segments.
func matchSegments(pat, name []string) bool {
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

// classNegation rewrites [!...] to the [^...] that path.Match reads.
func classNegation(s string) string {
	if !strings.Contains(s, "[!") {
		return s
	}
	b := []byte(s)
	inClass := false
	for i := 0; i < len(b); i++ {
		switch {
		case b[i] == '\\':
			i++
		case inClass:
			inClass = b[i] != ']'
		case b[i] == '[':
			inClass = true
			if i+1 < len(b) && b[i+1] == '!' {
				b[i+1] = '^'
				i++
			}
		}
	}
	return string(b)
}

// expand rewrites the first brace group into one pattern per alternative and
// recurses until none is left.
func expand(pattern string) ([]string, error) {
	open, closing, commas, err := firstGroup(pattern)
	if err != nil {
		return nil, err
	}
	if open < 0 {
		return []string{pattern}, nil
	}
	var out []string
	start := open + 1
	for _, end := range append(commas, closing) {
		more, err := expand(pattern[:open] + pattern[start:end] + pattern[closing+1:])
		if err != nil {
			return nil, err
		}
		if out = append(out, more...); len(out) > maxAlternatives {
			return nil, errTooManyBraces
		}
		start = end + 1
	}
	return out, nil
}

// firstGroup finds the first unescaped brace group outside a character class,
// with the positions of its top-level commas. open is -1 when there is none.
func firstGroup(s string) (open, closing int, commas []int, err error) {
	open = -1
	depth, inClass := 0, false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\\':
			i++
		case inClass:
			inClass = c != ']'
		case c == '[':
			inClass = true
		case c == '{':
			if depth == 0 {
				open = i
			}
			depth++
		case c == '}' && depth > 0:
			if depth--; depth == 0 {
				return open, i, commas, nil
			}
		case c == ',' && depth == 1:
			commas = append(commas, i)
		}
	}
	if depth > 0 {
		return -1, -1, nil, errUnclosedBrace
	}
	return -1, -1, nil, nil
}

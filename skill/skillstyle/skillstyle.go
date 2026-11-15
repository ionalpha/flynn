// Package skillstyle refuses marks an authored skill must not carry: machine-style
// punctuation, identifiers from the system that produced it, and internal names.
// It is code because a style rule kept by rereading does not survive twenty skills
// revised across many sessions.
//
// It does not judge vocabulary. Many tell words are ordinary English, and a check
// that fires on legitimate use gets switched off, so only marks with no legitimate
// use are refused.
package skillstyle

import (
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// Finding is one refusal, precise enough to fix without rereading the rule.
type Finding struct {
	// Path is the file, relative to the tree that was checked.
	Path string
	// Line and Column are 1-based; Column counts runes, as an editor does.
	Line, Column int
	// Match is the exact text that was refused.
	Match string
	// Rule names the rule, and Why says what to do instead.
	Rule, Why string
}

// String renders path:line:col, which editors and CI logs turn into a link.
func (f Finding) String() string {
	return fmt.Sprintf("%s:%d:%d: %s: %q. %s", f.Path, f.Line, f.Column, f.Rule, f.Match, f.Why)
}

type rule struct {
	name string
	why  string
	pat  *regexp.Regexp
}

// rules apply to every line in order. The dashes are separate rules because their
// replacements differ.
var rules = []rule{
	{
		name: "em-dash",
		why:  "Use a colon, a semicolon, parentheses, or a full stop.",
		pat:  regexp.MustCompile(`\x{2014}|\x{2015}`),
	},
	{
		name: "en-dash",
		why:  `Use a hyphen in a compound, or the word "to" in a range.`,
		pat:  regexp.MustCompile(`\x{2013}`),
	},
	{
		name: "ellipsis character",
		why:  "Write three dots.",
		pat:  regexp.MustCompile(`\x{2026}`),
	},
	{
		name: "internal record link",
		why:  "A reader outside this workspace cannot follow it. Say the thing instead of pointing at where it is recorded.",
		pat:  regexp.MustCompile(`@(?:task|note|epic|area|goal):[0-9a-fA-F-]{8,}`),
	},
	{
		name: "internal record id",
		why:  "A reader outside this workspace cannot follow it. Say the thing instead of pointing at where it is recorded.",
		pat:  regexp.MustCompile(`(?i)\b(?:task|note|epic|subtask|area|goal)\s+\x60?[0-9a-f]{8}\b`),
	},
	{
		name: "uuid",
		why:  "An identifier from the system that produced this skill means nothing to the person reading it.",
		pat:  regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`),
	},
	{
		name: "internal name",
		why:  "Name the practice, not the workspace it was worked out in.",
		pat:  regexp.MustCompile(`(?i)\bion\s+alpha\b|\bcortex\b`),
	},
	{
		name: "competitor",
		why:  "A skill teaches its craft. Comparing the shelf it sits on to another shelf dates immediately and reads as marketing.",
		pat:  regexp.MustCompile(`(?i)\bsuperpowers\b|\bskillsbench\b|\bskilljuror\b|\bclawhub\b`),
	},
}

// Check returns every refusal in content, ordered by position, and never stops at
// the first, so an author fixes them in one pass.
func Check(path string, content []byte) []Finding {
	var out []Finding
	for i, line := range strings.Split(string(content), "\n") {
		line = strings.TrimSuffix(line, "\r")
		for _, r := range rules {
			for _, loc := range r.pat.FindAllStringIndex(line, -1) {
				out = append(out, Finding{
					Path:   path,
					Line:   i + 1,
					Column: utf8.RuneCountInString(line[:loc[0]]) + 1,
					Match:  line[loc[0]:loc[1]],
					Rule:   r.name,
					Why:    r.why,
				})
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Line != out[j].Line {
			return out[i].Line < out[j].Line
		}
		return out[i].Column < out[j].Column
	})
	return out
}

// CheckFS checks every regular file under root in fsys, ordered by path, because
// reference documents reach the same reader as SKILL.md. An unreadable file is a
// finding, so it cannot end the walk and report a clean pack.
func CheckFS(fsys fs.FS, root string) ([]Finding, error) {
	var out []Finding
	err := fs.WalkDir(fsys, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		out = append(out, checkFile(fsys, p)...)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("skillstyle: walk %s: %w", root, err)
	}
	return out, nil
}

func checkFile(fsys fs.FS, p string) []Finding {
	b, err := fs.ReadFile(fsys, p)
	if err != nil {
		return []Finding{{
			Path: p, Line: 1, Column: 1, Match: p,
			Rule: "unreadable",
			Why:  "The gate could not read this file, so nothing here has been checked: " + err.Error(),
		}}
	}
	return Check(p, b)
}

// Report renders one finding per line, and "" for none.
func Report(findings []Finding) string {
	if len(findings) == 0 {
		return ""
	}
	lines := make([]string, len(findings))
	for i, f := range findings {
		lines[i] = f.String()
	}
	return strings.Join(lines, "\n")
}

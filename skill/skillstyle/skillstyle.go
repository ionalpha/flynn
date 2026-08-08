// Package skillstyle refuses the marks an authored skill must not carry: the
// punctuation that reads as machine-written, the identifiers that belong to the
// system that produced the skill rather than to the reader, and the internal names
// that were never meant to leave the workspace.
//
// # Why this is code and not a checklist
//
// A pack's whole argument is that a skill is a procedure with a proof rather than an
// opinion with confident prose. A style rule enforced by "re-read everything before
// the pull request" is an opinion about the prose, held by whoever is reading that
// day, which is the same shape as the thing the pack exists to replace. Twenty
// skills, each revised several times across many sessions, is the volume at which a
// hand-held rule stops holding.
//
// # What it does not do
//
// It does not judge vocabulary. Several of the words that mark generated text are
// ordinary English, and a check that fires on a legitimate use gets switched off
// inside a month, so that half needs an escape designed before it is worth having.
// What is here is the half with no legitimate use: three punctuation marks, the
// shape of an internal identifier, and a short list of names.
package skillstyle

import (
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// Finding is one refusal, addressed precisely enough to fix without rereading the
// rule. A report a reader cannot act on is a report that gets suppressed.
type Finding struct {
	// Path is the file, relative to the tree that was checked.
	Path string
	// Line and Column are 1-based, Column counted in runes so it lines up with what
	// an editor shows rather than with the byte offset.
	Line, Column int
	// Match is the exact text that was refused.
	Match string
	// Rule names the rule, and Why says what to do instead.
	Rule, Why string
}

// String renders a finding as one line, in the form editors and CI logs already
// know how to turn into a link.
func (f Finding) String() string {
	return fmt.Sprintf("%s:%d:%d: %s: %q. %s", f.Path, f.Line, f.Column, f.Rule, f.Match, f.Why)
}

// rule is one refused pattern with the advice that goes with it.
type rule struct {
	name string
	why  string
	pat  *regexp.Regexp
}

// rules are applied to every line of every file in a pack, in this order.
//
// The dashes are three separate refusals rather than one character class, because
// the replacement differs: an em-dash wants a colon, a semicolon, parentheses or a
// full stop, while a spaced en-dash between numbers wants "to".
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

// Check returns every refusal in content, addressed against path. It reads line by
// line so a finding carries a position, and it never stops at the first: an author
// fixing one mark at a time across twenty skills is the cost this exists to avoid.
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

// CheckFS returns every refusal in every regular file under root in fsys, ordered by
// path. Everything in a skill directory is checked, not only SKILL.md: a reference
// document is read by the same person and carries the same marks.
//
// An unreadable file is a finding of its own rather than an error return, so one
// broken file cannot hide the twenty findings in the files beside it.
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

// checkFile returns the refusals in one file, or the single finding that says the
// file could not be read. Reading is a finding rather than a failure so that one
// broken path cannot end the walk and report a clean pack.
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

// Report renders findings as one line each, ready to hand to a test failure. It
// returns "" for no findings, so a caller can test the report rather than the count.
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

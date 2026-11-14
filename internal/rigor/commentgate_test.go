package rigor_test

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const (
	// commentBlockBudget is the most lines a comment block may have anywhere
	// but a package comment. A block longer than this is usually restating the
	// code or arguing a design that belongs in a document.
	commentBlockBudget = 14
	// packageDocBudget is the larger allowance for a package comment, which is
	// the package's reference page and may carry its overview.
	packageDocBudget = 40
)

// commentRule is one thing a comment may not contain. match returns how many
// times the comment text breaks the rule.
type commentRule struct {
	name  string
	match func(text string) int
}

// phraseRule counts matches of a case-insensitive pattern in comment text that
// has had its line breaks and comment markers folded into single spaces.
func phraseRule(name, pattern string) commentRule {
	re := regexp.MustCompile(`(?i)` + pattern)
	return commentRule{name, func(text string) int { return len(re.FindAllStringIndex(text, -1)) }}
}

// commentRules are the rules the gate applies to every comment of shipped code.
// The phrase patterns skip the literal uses of the words ("no longer valid",
// "previously stored", "used to build the index", "the order matters") so that
// a hit is an instance of the habit and the gate stays worth leaving on.
var commentRules = []commentRule{
	phraseRule("dash", `[\x{2014}\x{2013}\x{2026}]`),
	phraseRule("filler", `\b(load-bearing|seamless(ly)?|leverag(e|es|ed|ing)|delve[sd]?|crucially|importantly|quietly|in essence|it'?s worth noting|worth noting|proven:)`),
	phraseRule("deliberately", `\bdeliberately\b`),
	phraseRule("matters", `\b(this|that|which|it|distinction|difference) matters\b`),
	phraseRule("history", `\bused to be\b|\b(it|this|that|which|we|they) used to\b|\bwas changed\b|\bwe now\b|\bas of this change\b|\bno longer (is|are|was|were) (a|an|the)\b`),
}

// lengthRule is the name under which an over-budget block is baselined.
const lengthRule = "length"

// baselinePath is the checked-in list of violations that predate the gate.
const baselinePath = "testdata/comment_baseline.txt"

// TestCommentsStayTerse fails on a comment in production Go code that breaks
// one of the writing rules in AGENTS.md: a dash or ellipsis character, a
// filler word, a history phrase, or a block longer than the line budget.
//
// Doc comments are the published API reference, and a comment that tells how
// the code used to be, or that pads a point with an intensifier, costs every
// reader of that page and goes stale first. The rules are mechanical so that a
// reviewer does not have to be the one to say so.
//
// Violations that predate the gate are listed in testdata/comment_baseline.txt,
// one "<file> <rule> <count>" line each, keyed by count and not by line number
// so that an edit elsewhere in the file does not disturb it. The test requires
// every file's count for every rule to equal its baseline entry exactly: a new
// violation fails it, and so does an entry whose count has dropped, which
// forces the cleanup to lower or delete the line in the same change. The file
// can therefore only shrink. Test files, generated files and testdata are not
// checked.
func TestCommentsStayTerse(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("locate the repository root: %v", err)
	}
	got := commentViolations(t, root)
	want := readCommentBaseline(t, baselinePath)

	var problems []string
	for key, v := range got {
		switch w, ok := want[key]; {
		case !ok:
			problems = append(problems, fmt.Sprintf("new: %s: %d (%s)", key, v.n, strings.Join(v.where, "; ")))
		case v.n > w:
			problems = append(problems, fmt.Sprintf("grew: %s: %d, baseline %d (%s)", key, v.n, w, strings.Join(v.where, "; ")))
		case v.n < w:
			problems = append(problems, fmt.Sprintf("shrank: %s: %d, baseline %d: lower the baseline entry to %d", key, v.n, w, v.n))
		}
	}
	for key, w := range want {
		if _, ok := got[key]; !ok {
			problems = append(problems, fmt.Sprintf("stale: %s: baseline %d but no violation remains: delete the line", key, w))
		}
	}
	slices.Sort(problems)
	if len(problems) > 0 {
		t.Errorf("comment rules (see AGENTS.md, Comments):\n  %s", strings.Join(problems, "\n  "))
	}
}

// TestCommentRulesPinned pins what each rule counts and, as importantly, what
// it leaves alone, so a loosened pattern cannot start flagging literal uses.
func TestCommentRulesPinned(t *testing.T) {
	cases := []struct {
		rule string
		text string
		want int
	}{
		{"dash", "a — b", 1},
		{"dash", "a - b, 1-2", 0},
		{"filler", "this is quietly wrong", 1},
		{"filler", "the load-bearing check", 1},
		{"filler", "a quiet period", 0},
		{"deliberately", "deliberately left open", 1},
		{"deliberately", "deliberate choice", 0},
		{"matters", "that distinction matters here", 1},
		{"matters", "the order matters", 0},
		{"matters", "a matters list", 0},
		{"history", "a cache that is no longer valid", 0},
		{"history", "it used to be a map", 1},
		{"history", "used to build the index", 0},
		{"history", "previously stored", 0},
		{"history", "this was changed to a map", 1},
	}
	rules := map[string]commentRule{}
	for _, r := range commentRules {
		rules[r.name] = r
	}
	for _, c := range cases {
		if got := rules[c.rule].match(c.text); got != c.want {
			t.Errorf("%s on %q = %d, want %d", c.rule, c.text, got, c.want)
		}
	}
}

// TestCommentFileScanSkipsGenerated pins the scanner: it reads comments of
// ordinary files, applies the longer budget to the package comment, and skips
// a file marked as generated.
func TestCommentFileScanSkipsGenerated(t *testing.T) {
	long := strings.Repeat("// line\n", commentBlockBudget+1)
	cases := []struct {
		name string
		src  string
		want map[string]int
	}{
		{"plain", "package p\n\n// quietly\nfunc F() {}\n", map[string]int{"filler": 1}},
		{"long block", "package p\n\n" + long + "func F() {}\n", map[string]int{lengthRule: 1}},
		{"long package doc within budget", long + "package p\n", map[string]int{}},
		{"generated", "// Code generated by x; DO NOT EDIT.\n\npackage p\n\n// quietly\nfunc F() {}\n", map[string]int{}},
	}
	for _, c := range cases {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "p.go", c.src, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]int{}
		for rule, v := range scanComments(fset, f) {
			got[rule] = v.n
		}
		if len(got) != len(c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
			continue
		}
		for rule, n := range c.want {
			if got[rule] != n {
				t.Errorf("%s: got %v, want %v", c.name, got, c.want)
			}
		}
	}
}

// violation is the count of one rule in one file, with the first few places.
type violation struct {
	n     int
	where []string
}

// commentViolations scans every shipped Go file under root and returns the
// violations keyed "<file> <rule>".
func commentViolations(t *testing.T, root string) map[string]violation {
	t.Helper()
	out := map[string]violation{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != root && skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, p, nil, parser.ParseComments)
		if err != nil {
			return fmt.Errorf("parse %s: %w", rel, err)
		}
		for rule, v := range scanComments(fset, f) {
			out[rel+" "+rule] = v
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}

// skipDir names the directories the gate does not read.
func skipDir(name string) bool {
	switch name {
	case "vendor", "testdata", "node_modules":
		return true
	}
	return strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")
}

// generatedMarker is the line the go command documents for generated files.
var generatedMarker = regexp.MustCompile(`^// Code generated .* DO NOT EDIT\.$`)

// scanComments returns the violations of one parsed file by rule.
func scanComments(fset *token.FileSet, f *ast.File) map[string]violation {
	out := map[string]violation{}
	for _, g := range f.Comments {
		if g.End() < f.Package && isGenerated(g) {
			return map[string]violation{}
		}
	}
	add := func(rule string, n int, where string) {
		v := out[rule]
		v.n += n
		if len(v.where) < 3 {
			v.where = append(v.where, where)
		}
		out[rule] = v
	}
	for _, g := range f.Comments {
		text := g.Text()
		line := fset.Position(g.Pos()).Line
		where := "line " + strconv.Itoa(line)

		budget := commentBlockBudget
		if g == f.Doc {
			budget = packageDocBudget
		}
		if lines := strings.Count(text, "\n"); lines > budget {
			add(lengthRule, 1, where)
		}

		folded := strings.Join(strings.Fields(text), " ")
		for _, r := range commentRules {
			if n := r.match(folded); n > 0 {
				add(r.name, n, where)
			}
		}
	}
	return out
}

func isGenerated(g *ast.CommentGroup) bool {
	for _, c := range g.List {
		if generatedMarker.MatchString(c.Text) {
			return true
		}
	}
	return false
}

// readCommentBaseline parses the baseline file into "<file> <rule>" -> count.
func readCommentBaseline(t *testing.T, name string) map[string]int {
	t.Helper()
	fh, err := os.Open(name)
	if err != nil {
		t.Fatalf("open baseline: %v", err)
	}
	defer func() { _ = fh.Close() }()
	out := map[string]int{}
	sc := bufio.NewScanner(fh)
	for ln := 1; sc.Scan(); ln++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) != 3 {
			t.Fatalf("%s:%d: want \"<file> <rule> <count>\", got %q", name, ln, line)
		}
		n, err := strconv.Atoi(parts[2])
		if err != nil || n < 1 {
			t.Fatalf("%s:%d: count %q is not a positive integer", name, ln, parts[2])
		}
		key := parts[0] + " " + parts[1]
		if _, dup := out[key]; dup {
			t.Fatalf("%s:%d: duplicate entry %q", name, ln, key)
		}
		out[key] = n
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read baseline: %v", err)
	}
	return out
}

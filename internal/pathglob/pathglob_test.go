package pathglob

import (
	"strings"
	"testing"
)

func TestMatch(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		{"**", "a/b/c", true},
		{"**/x", "x", true},
		{"**/x", "a/b/x", true},
		{"a/**/z", "a/z", true},
		{"a/**/z", "a/b/c/z", true},
		{"a/**/z", "a/b/c", false},
		{"a/**", "a", true},
		{"a**b", "a/b", false},
		{"*.go", "main.go", true},
		{"*.go", "sub/main.go", false},
		{"{a,b}/{c,d{e,f}}", "b/df", true},
		{"{a,b/c}/x", "b/c/x", true},
		{"{a}", "a", true},
		{`\{a,b\}`, "{a,b}", true},
		{"[{]x", "{x", true},
		{"a}", "a}", true},
		{"[!a-c]", "d", true},
		{"[!a-c]", "b", false},
		{`[a\]!]`, "!", true},
	}
	for _, tc := range cases {
		p, err := Compile(tc.pattern)
		if err != nil {
			t.Errorf("Compile(%q): %v", tc.pattern, err)
			continue
		}
		if got := p.Match(tc.name); got != tc.want {
			t.Errorf("Match(%q, %q) = %v, want %v", tc.pattern, tc.name, got, tc.want)
		}
	}
}

func TestCompileRejects(t *testing.T) {
	for _, pattern := range []string{"{a,b", "a/{b,{c}", "[", "x/[a", strings.Repeat("{a,b}", 11)} {
		if _, err := Compile(pattern); err == nil {
			t.Errorf("Compile(%q) succeeded", pattern)
		}
	}
}

// TestMatchRepeatedDoublestar pins the cost of stacking ** segments: a
// backtracking matcher is exponential here and runs past the test deadline.
func TestMatchRepeatedDoublestar(t *testing.T) {
	p, err := Compile(strings.Repeat("**/", 16) + "z")
	if err != nil {
		t.Fatal(err)
	}
	deep := strings.TrimSuffix(strings.Repeat("a/", 40), "/")
	if p.Match(deep) || !p.Match(deep+"/z") {
		t.Error("deep path matched wrongly")
	}
}

func TestMayContain(t *testing.T) {
	cases := []struct {
		pattern, dir string
		want         bool
	}{
		{"*.go", "a", false},
		{"a/*.go", "a", true},
		{"a/*.go", "b", false},
		{"a/*.go", "a/b", false},
		{"a/**/*.go", "a/b/c", true},
		{"{x,a/b}/*", "a", true},
	}
	for _, tc := range cases {
		p, err := Compile(tc.pattern)
		if err != nil {
			t.Fatal(err)
		}
		if got := p.MayContain(tc.dir); got != tc.want {
			t.Errorf("MayContain(%q, %q) = %v, want %v", tc.pattern, tc.dir, got, tc.want)
		}
	}
}

func TestIgnoreDecide(t *testing.T) {
	ig := ParseIgnore([]byte("\ufeff# c\n*.log\n!keep.log\nbuild/**\n!build/keep\ndocs/*.md\ntrail\\ \nspaced   \n\\#hash\n/\n[!a]z\n"))
	cases := []struct {
		rel              string
		isDir            bool
		ignored, matched bool
	}{
		{"a/x.log", false, true, true},
		{"a/keep.log", false, false, true},
		{"build", true, false, false},
		{"build/o", false, true, true},
		{"build/keep", false, false, true},
		{"docs/a.md", false, true, true},
		{"x/docs/a.md", false, false, false},
		{"trail ", false, true, true},
		{"spaced", false, true, true},
		{"#hash", false, true, true},
		{"bz", false, true, true},
		{"az", false, false, false},
		{"", true, false, false},
	}
	for _, tc := range cases {
		ignored, matched := ig.Decide(tc.rel, tc.isDir)
		if ignored != tc.ignored || matched != tc.matched {
			t.Errorf("Decide(%q) = %v, %v; want %v, %v", tc.rel, ignored, matched, tc.ignored, tc.matched)
		}
	}
}

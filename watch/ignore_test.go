package watch

import (
	"strings"
	"testing"
)

func TestIgnoreMatch(t *testing.T) {
	ig := ParseIgnore([]byte(`
# a comment
node_modules/
*.log
/dist
build
**/tmp
docs/*.md
!docs/README.md
`))

	cases := []struct {
		rel   string
		isDir bool
		want  bool
	}{
		{rel: "node_modules", isDir: true, want: true},
		{rel: "src/node_modules", isDir: true, want: true}, // unanchored dir at any depth
		{rel: "node_modules", isDir: false, want: false},   // dir-only rule ignores files
		{rel: "server.log", want: true},
		{rel: "logs/app.log", want: true}, // *.log matches basename at any depth
		{rel: "dist", isDir: true, want: true},
		{rel: "src/dist", isDir: true, want: false}, // /dist is anchored to root
		{rel: "build", isDir: true, want: true},
		{rel: "src/build", isDir: true, want: true}, // build unanchored
		{rel: "a/b/tmp", isDir: true, want: true},   // **/tmp
		{rel: "tmp", isDir: true, want: true},
		{rel: "docs/guide.md", want: true},
		{rel: "docs/README.md", want: false}, // re-included by negation
		{rel: "main.go", want: false},
	}
	for _, tc := range cases {
		if got := ig.Match(tc.rel, tc.isDir); got != tc.want {
			t.Errorf("Match(%q, dir=%v) = %v, want %v", tc.rel, tc.isDir, got, tc.want)
		}
	}
}

func TestIgnoreEmpty(t *testing.T) {
	ig := ParseIgnore(nil)
	if ig.Match("anything.go", false) {
		t.Error("empty ignore should match nothing")
	}
}

func TestMatchGlobDoublestar(t *testing.T) {
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
		{"*.go", "main.go", true},
		{"*.go", "sub/main.go", false},
	}
	for _, tc := range cases {
		if got := matchGlob(tc.pattern, tc.name); got != tc.want {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", tc.pattern, tc.name, got, tc.want)
		}
	}
}

// TestMatchGlobRepeatedDoublestar pins the cost of a pattern stacking many **
// segments: the recursive matcher this replaced backtracked exponentially and
// hung the fuzz smoke on inputs like these. With the dynamic program the case
// finishes instantly; a regression re-hangs the test past its deadline.
func TestMatchGlobRepeatedDoublestar(t *testing.T) {
	pattern := strings.TrimSuffix(strings.Repeat("**/", 16), "/") + "/z"
	deep := strings.TrimSuffix(strings.Repeat("a/", 40), "/")
	if matchGlob(pattern, deep) {
		t.Errorf("matchGlob(%q, deep path) = true, want false", pattern)
	}
	if !matchGlob(pattern, deep+"/z") {
		t.Errorf("matchGlob(%q, deep path ending in z) = false, want true", pattern)
	}
}

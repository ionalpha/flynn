package watch

import (
	"path"
	"strings"
	"testing"
)

// FuzzParseIgnore checks that parsing ignore-file content and matching against it
// is total: no bytes crash the parser, and Match never panics for any resulting
// pattern set and any path. Ignore files come from the watched tree, so hostile or
// malformed content must degrade to a usable matcher, not a crash. Match must also
// be deterministic and insensitive to surrounding slashes, since a watcher decides
// whether to skip a path by calling it on whatever relative path the walk produced.
func FuzzParseIgnore(f *testing.F) {
	seeds := []struct {
		content string
		rel     string
		isDir   bool
	}{
		{"", "a.txt", false},
		{"*.log\n", "x/y.log", false},
		{"# comment\n/build/\n!keep.txt\n", "build", true},
		{"\n\n\n", "", false},
		{"a/b/c\n**/node_modules\n", "a/b/c", false},
		{"!!!\n[bad", "weird[", false},
		{"\x00\x01\n", "\x00", false},
		{"   \t  \n", "  ", true},
		{"!keep.txt\n*.txt\n", "keep.txt", false},
	}
	for _, s := range seeds {
		f.Add([]byte(s.content), s.rel, s.isDir)
	}

	f.Fuzz(func(t *testing.T, content []byte, rel string, isDir bool) {
		ig := ParseIgnore(content)
		if ig == nil {
			t.Fatal("ParseIgnore returned nil; it must always yield a usable matcher")
		}
		got := ig.Match(rel, isDir)
		if again := ig.Match(rel, isDir); again != got {
			t.Fatalf("Match(%q, %v) is not deterministic: %v then %v", rel, isDir, got, again)
		}
		// Match cleans the path before testing it, so decorating an already-clean
		// relative path with leading and trailing separators cannot change the
		// verdict: a walk that produced "build/" must be skipped exactly when "build"
		// is. Parent traversal is excluded because path.Clean resolves ".." against a
		// leading slash but preserves it on a relative path, so the two spellings
		// legitimately denote different paths. A tree walk never yields one.
		slashed := strings.ReplaceAll(rel, "\\", "/")
		clean := path.Clean(slashed)
		if clean == slashed && clean != "." && !strings.HasPrefix(clean, "..") {
			if padded := ig.Match("/"+rel+"/", isDir); padded != got {
				t.Fatalf("Match(%q, %v)=%v but Match(%q, %v)=%v", rel, isDir, got, "/"+rel+"/", isDir, padded)
			}
		}
	})
}

// FuzzScanLine checks that marker scanning is total over any line and honours the
// contract ScanLine documents: ok is false when the line holds no marker or the
// instruction is empty, and code is the source text that preceded the comment.
func FuzzScanLine(f *testing.F) {
	for _, s := range []string{
		"", "// flynn: keep", "plain code", "###", "\t spaced", "no marker",
		"x := 1 // ai! do the thing", "<!-- ai? why -->", "/* ai! go */",
		"// ai!", "// ai!!", "// airplane", "; ai? q", "-- ai! sql",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, line string) {
		kind, instruction, code, ok := ScanLine(line)
		if !ok {
			if instruction != "" || code != "" || kind != "" {
				t.Fatalf("ScanLine(%q) reported no marker but returned kind=%q instruction=%q code=%q", line, kind, instruction, code)
			}
			return
		}
		if instruction == "" {
			t.Fatalf("ScanLine(%q) reported a marker with an empty instruction", line)
		}
		if kind != Act && kind != Ask {
			t.Fatalf("ScanLine(%q) reported an unknown marker kind %q", line, kind)
		}
		if !strings.HasPrefix(line, code) {
			t.Fatalf("ScanLine(%q) returned code %q that does not lead the line", line, code)
		}
	})
}

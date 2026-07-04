package main

import (
	"os"
	"path/filepath"
	"testing"
)

// completionTree builds a small project tree for completer tests.
func completionTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{".git/objects", "node_modules/pkg", "src", "docs"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range []string{
		".git/objects/aa",
		"node_modules/pkg/index.js",
		".gitignore",
		"src/main.go",
		"src/main_test.go",
		"docs/readme.md",
	} {
		if err := os.WriteFile(filepath.Join(root, file), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestListFilesSkipsDotDirsAndDependencyTrees(t *testing.T) {
	got := listFiles(completionTree(t), fileCap)
	want := map[string]bool{
		".gitignore":       true,
		"src/main.go":      true,
		"src/main_test.go": true,
		"docs/readme.md":   true,
	}
	if len(got) != len(want) {
		t.Fatalf("listFiles = %v; want the keys of %v", got, want)
	}
	for _, g := range got {
		if !want[g] {
			t.Fatalf("listFiles included %q", g)
		}
	}
}

func TestListFilesHonorsTheCap(t *testing.T) {
	got := listFiles(completionTree(t), 2)
	if len(got) != 2 {
		t.Fatalf("listFiles returned %d entries, want the cap of 2", len(got))
	}
}

func TestCompleteRanksByQuery(t *testing.T) {
	fc := newFileCompleter(completionTree(t))
	got := fc.Complete("readme")
	if len(got) == 0 || got[0] != "docs/readme.md" {
		t.Fatalf("Complete(readme) = %v", got)
	}
}

func TestAcceptedPickRisesToTheTop(t *testing.T) {
	fc := newFileCompleter(completionTree(t))
	before := fc.Complete("main")
	if len(before) < 2 || before[0] != "src/main.go" {
		t.Fatalf("Complete(main) = %v; want src/main.go first", before)
	}
	fc.Accepted("src/main_test.go")
	after := fc.Complete("main")
	if after[0] != "src/main_test.go" {
		t.Fatalf("Complete(main) after pick = %v; want the picked file first", after)
	}
}

// TestFrecencyFavorsTheRecentPick pins the recency half of frecency: a file
// picked once, most recently, outranks one picked more times but longer ago.
// Frequency alone would keep the thrice-picked file on top; recency flips it.
func TestFrecencyFavorsTheRecentPick(t *testing.T) {
	fc := newFileCompleter(completionTree(t))
	for range 3 {
		fc.Accepted("src/main.go")
	}
	fc.Accepted("src/main_test.go") // once, but the most recent

	got := fc.Complete("main")
	if len(got) < 2 || got[0] != "src/main_test.go" {
		t.Fatalf("Complete(main) = %v; want the most recently picked file first", got)
	}
}

// TestFrecencyKeepsFrequencyWeight guards the other half: frequency still
// carries weight. A file picked many times outranks one picked once more
// recently, so the recency term does not erase the frequency signal.
func TestFrecencyKeepsFrequencyWeight(t *testing.T) {
	fc := newFileCompleter(completionTree(t))
	for range 5 {
		fc.Accepted("src/main_test.go")
	}
	fc.Accepted("src/main.go") // once, most recent, but far less frequent

	got := fc.Complete("main")
	if len(got) < 2 || got[0] != "src/main_test.go" {
		t.Fatalf("Complete(main) = %v; want the much more frequently picked file first", got)
	}
}

func TestEmptyQueryRefreshesTheUniverse(t *testing.T) {
	root := completionTree(t)
	fc := newFileCompleter(root)
	if got := fc.Complete("fresh"); len(got) != 0 {
		t.Fatalf("Complete(fresh) = %v before the file exists", got)
	}
	if err := os.WriteFile(filepath.Join(root, "fresh.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A non-empty query reuses the cached walk; the empty query (a new
	// completion session) re-walks and sees the new file.
	if got := fc.Complete("fresh"); len(got) != 0 {
		t.Fatalf("cached walk unexpectedly refreshed: %v", got)
	}
	fc.Complete("")
	got := fc.Complete("fresh")
	if len(got) != 1 || got[0] != "fresh.go" {
		t.Fatalf("Complete(fresh) after refresh = %v", got)
	}
}

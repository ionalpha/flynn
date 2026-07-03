package term_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ionalpha/flynn/internal/tui/term"
)

func TestEditorCommandPrefersVisualThenEditorThenDefault(t *testing.T) {
	t.Setenv("VISUAL", "code --wait")
	t.Setenv("EDITOR", "vim")
	if got := term.EditorCommand(); len(got) != 2 || got[0] != "code" || got[1] != "--wait" {
		t.Fatalf("with VISUAL set, got %q, want [code --wait]", got)
	}

	t.Setenv("VISUAL", "")
	if got := term.EditorCommand(); len(got) != 1 || got[0] != "vim" {
		t.Fatalf("with only EDITOR set, got %q, want [vim]", got)
	}

	t.Setenv("EDITOR", "")
	if got := term.EditorCommand(); len(got) == 0 {
		t.Fatal("with neither set, want a platform default, got nothing")
	}
}

// TestRunAttachedRunsTheEditorOverTheFile spawns a real child process as the
// editor and checks it edited the file it was handed. The child is this test
// binary re-invoked (portable across every OS the suite runs on): it appends
// to the path in its last argument, standing in for what an editor does to
// the buffer.
func TestRunAttachedRunsTheEditorOverTheFile(t *testing.T) {
	if os.Getenv("GO_WANT_EDITOR_HELPER") == "1" {
		// Running as the child editor: append to the file we were handed.
		path := os.Args[len(os.Args)-1]
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			os.Exit(1)
		}
		_, werr := f.WriteString(" edited")
		cerr := f.Close()
		if werr != nil || cerr != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "draft.md")
	if err := os.WriteFile(path, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}

	editor := []string{os.Args[0], "-test.run=TestRunAttachedRunsTheEditorOverTheFile"}
	t.Setenv("GO_WANT_EDITOR_HELPER", "1")
	if err := term.RunAttached(append(editor, path)); err != nil {
		t.Fatalf("RunAttached: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello edited" {
		t.Fatalf("file = %q, want %q; the editor handoff did not run over the file", got, "hello edited")
	}
}

// TestRunAttachedReportsAMissingEditor surfaces the spawn error rather than
// swallowing it, so a bad $EDITOR is reported to the user instead of silently
// doing nothing.
func TestRunAttachedReportsAMissingEditor(t *testing.T) {
	if err := term.RunAttached([]string{"flynn-no-such-editor-xyzzy"}); err == nil {
		t.Fatal("running a nonexistent editor returned nil, want an error")
	}
}

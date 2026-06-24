package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/mission"
)

func newSet(t *testing.T) *Set {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func invoke(t *testing.T, tool mission.Tool, input any) (string, error) {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	return tool.Invoke(context.Background(), raw)
}

// writeFile drops a file under the set's root via the OS, bypassing the tools, so
// tests can set up fixtures.
func writeFile(t *testing.T, s *Set, rel, content string) {
	t.Helper()
	p := filepath.Join(s.root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestWriteThenRead(t *testing.T) {
	s := newSet(t)
	if _, err := invoke(t, writeTool{s}, map[string]any{"path": "sub/a.txt", "content": "hello\nworld\n"}); err != nil {
		t.Fatal(err)
	}
	got, err := invoke(t, readTool{s}, map[string]any{"path": "sub/a.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "hello\nworld\n" {
		t.Fatalf("read back %q", got)
	}
	// Offset/limit slice lines (1-based offset).
	got, err = invoke(t, readTool{s}, map[string]any{"path": "sub/a.txt", "offset": 2, "limit": 1})
	if err != nil {
		t.Fatal(err)
	}
	if got != "world" {
		t.Fatalf("offset/limit read = %q, want %q", got, "world")
	}
}

func TestEditSingleMatchContract(t *testing.T) {
	s := newSet(t)
	writeFile(t, s, "f.txt", "alpha beta alpha")

	// Multiple matches: rejected (ambiguous).
	if _, err := invoke(t, editTool{s}, map[string]any{"path": "f.txt", "old": "alpha", "new": "X"}); err == nil {
		t.Fatal("edit with 2 matches should fail")
	}
	// Zero matches: rejected.
	if _, err := invoke(t, editTool{s}, map[string]any{"path": "f.txt", "old": "zzz", "new": "X"}); err == nil {
		t.Fatal("edit with 0 matches should fail")
	}
	// Empty old: rejected.
	if _, err := invoke(t, editTool{s}, map[string]any{"path": "f.txt", "old": "", "new": "X"}); err == nil {
		t.Fatal("edit with empty old should fail")
	}
	// Exactly one match: succeeds.
	if _, err := invoke(t, editTool{s}, map[string]any{"path": "f.txt", "old": "beta", "new": "BETA"}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(s.root, "f.txt")); string(b) != "alpha BETA alpha" {
		t.Fatalf("edit result = %q", b)
	}
}

func TestGlob(t *testing.T) {
	s := newSet(t)
	writeFile(t, s, "a.go", "")
	writeFile(t, s, "b.go", "")
	writeFile(t, s, "c.txt", "")
	got, err := invoke(t, globTool{s}, map[string]any{"pattern": "*.go"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "a.go\nb.go" {
		t.Fatalf("glob = %q, want a.go\\nb.go", got)
	}
	if got, _ := invoke(t, globTool{s}, map[string]any{"pattern": "*.none"}); got != "no matches" {
		t.Fatalf("empty glob = %q", got)
	}
}

func TestGrep(t *testing.T) {
	s := newSet(t)
	writeFile(t, s, "src/x.go", "package x\nfunc Foo() {}\n")
	writeFile(t, s, "src/y.go", "package y\nfunc Bar() {}\n")
	writeFile(t, s, "bin/blob", "\x00\x01func Foo") // binary: must be skipped

	got, err := invoke(t, grepTool{s}, map[string]any{"pattern": `func \w+\(`})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "src/x.go:2:func Foo() {}") || !strings.Contains(got, "src/y.go:2:func Bar() {}") {
		t.Fatalf("grep missed matches:\n%s", got)
	}
	if strings.Contains(got, "blob") {
		t.Fatalf("grep did not skip the binary file:\n%s", got)
	}
	// Scoped search.
	got, _ = invoke(t, grepTool{s}, map[string]any{"pattern": "Foo", "path": "src/x.go"})
	if !strings.Contains(got, "x.go") || strings.Contains(got, "y.go") {
		t.Fatalf("scoped grep = %q", got)
	}
}

func TestPathConfinementRejectsEscapes(t *testing.T) {
	s := newSet(t)
	writeFile(t, s, "in.txt", "inside")
	// Plant a secret outside the root.
	outside := filepath.Join(filepath.Dir(s.root), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Remove(outside) }()

	for _, p := range []string{"../secret.txt", "../../secret.txt", "sub/../../secret.txt"} {
		if _, err := invoke(t, readTool{s}, map[string]any{"path": p}); err == nil {
			t.Fatalf("read of %q should have been rejected as an escape", p)
		}
		if _, err := invoke(t, writeTool{s}, map[string]any{"path": p, "content": "x"}); err == nil {
			t.Fatalf("write of %q should have been rejected as an escape", p)
		}
	}
}

func TestPathConfinementRejectsSymlinkEscape(t *testing.T) {
	s := newSet(t)
	target := filepath.Join(filepath.Dir(s.root), "outside.txt")
	if err := os.WriteFile(target, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Remove(target) }()
	link := filepath.Join(s.root, "link.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable on this platform/run: %v", err)
	}
	if _, err := invoke(t, readTool{s}, map[string]any{"path": "link.txt"}); err == nil {
		t.Fatal("reading through a symlink that points outside the root must be rejected")
	}
}

// TestConfinementProperty is the safety invariant: for any model-supplied path
// (relative, absolute, or studded with ".."), resolve either rejects it or returns
// a path that stays within the root. It never yields a path that escapes.
func TestConfinementProperty(t *testing.T) {
	s := newSet(t)
	seg := rapid.SampledFrom([]string{"..", "a", "b", "sub", "x.txt", ".", "deep"})
	rapid.Check(t, func(rt *rapid.T) {
		parts := rapid.SliceOfN(seg, 1, 6).Draw(rt, "parts")
		p := strings.Join(parts, "/")
		if rapid.Bool().Draw(rt, "absolute") {
			p = "/" + p
		}
		abs, err := s.resolve(p)
		if err != nil {
			return // rejected: safe
		}
		if !within(s.root, abs) {
			rt.Fatalf("resolve(%q) escaped the root: %q (root %q)", p, abs, s.root)
		}
	})
}

func TestBash(t *testing.T) {
	s := newSet(t)
	out, err := invoke(t, bashTool{s}, map[string]any{"command": "echo hello"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "hello") {
		t.Fatalf("bash echo = %q", out)
	}
	// A non-zero exit returns the output and exit code, not a Go error.
	out, err = invoke(t, bashTool{s}, map[string]any{"command": "exit 7"})
	if err != nil {
		t.Fatalf("non-zero exit should not be a tool error: %v", err)
	}
	if !strings.Contains(out, "exit status 7") {
		t.Fatalf("bash exit code not reported: %q", out)
	}
	// Empty command is a usage error.
	if _, err := invoke(t, bashTool{s}, map[string]any{"command": ""}); err == nil {
		t.Fatal("empty command should error")
	}
}

func TestToolsExposesFullSet(t *testing.T) {
	s := newSet(t)
	names := map[string]bool{}
	for _, tool := range s.Tools() {
		names[tool.Def().Name] = true
	}
	for _, want := range []string{"bash", "read", "write", "edit", "glob", "grep"} {
		if !names[want] {
			t.Fatalf("default toolset missing %q; have %v", want, names)
		}
	}
}

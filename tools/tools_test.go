package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/sandbox"
)

func newSet(t *testing.T) (*Set, sandbox.Sandbox) {
	t.Helper()
	sb, err := sandbox.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return New(sb), sb
}

func invoke(t *testing.T, tool mission.Tool, input any) (string, error) {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	return tool.Invoke(context.Background(), raw)
}

// seed writes a file through the sandbox, bypassing the tools, for fixtures.
func seed(t *testing.T, sb sandbox.Sandbox, path, content string) {
	t.Helper()
	if err := sb.WriteFile(context.Background(), path, []byte(content)); err != nil {
		t.Fatal(err)
	}
}

func TestWriteThenRead(t *testing.T) {
	s, _ := newSet(t)
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
	got, err = invoke(t, readTool{s}, map[string]any{"path": "sub/a.txt", "offset": 2, "limit": 1})
	if err != nil {
		t.Fatal(err)
	}
	if got != "world" {
		t.Fatalf("offset/limit read = %q, want %q", got, "world")
	}
}

func TestEditSingleMatchContract(t *testing.T) {
	s, sb := newSet(t)
	seed(t, sb, "f.txt", "alpha beta alpha")

	if _, err := invoke(t, editTool{s}, map[string]any{"path": "f.txt", "old": "alpha", "new": "X"}); err == nil {
		t.Fatal("edit with 2 matches should fail")
	}
	if _, err := invoke(t, editTool{s}, map[string]any{"path": "f.txt", "old": "zzz", "new": "X"}); err == nil {
		t.Fatal("edit with 0 matches should fail")
	}
	if _, err := invoke(t, editTool{s}, map[string]any{"path": "f.txt", "old": "", "new": "X"}); err == nil {
		t.Fatal("edit with empty old should fail")
	}
	if _, err := invoke(t, editTool{s}, map[string]any{"path": "f.txt", "old": "beta", "new": "BETA"}); err != nil {
		t.Fatal(err)
	}
	got, _ := invoke(t, readTool{s}, map[string]any{"path": "f.txt"})
	if got != "alpha BETA alpha" {
		t.Fatalf("edit result = %q", got)
	}
}

func TestGlob(t *testing.T) {
	s, sb := newSet(t)
	seed(t, sb, "a.go", "")
	seed(t, sb, "b.go", "")
	seed(t, sb, "c.txt", "")
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
	s, sb := newSet(t)
	seed(t, sb, "src/x.go", "package x\nfunc Foo() {}\n")
	seed(t, sb, "src/y.go", "package y\nfunc Bar() {}\n")
	seed(t, sb, "bin/blob", "\x00\x01func Foo") // binary: must be skipped

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
	got, _ = invoke(t, grepTool{s}, map[string]any{"pattern": "Foo", "path": "src/x.go"})
	if !strings.Contains(got, "x.go") || strings.Contains(got, "y.go") {
		t.Fatalf("scoped grep = %q", got)
	}
}

func TestToolsRejectEscapes(t *testing.T) {
	s, _ := newSet(t)
	// The sandbox denies the escape; the tool surfaces it as an error.
	if _, err := invoke(t, readTool{s}, map[string]any{"path": "../secret.txt"}); err == nil {
		t.Fatal("reading outside the sandbox should be rejected")
	}
	if _, err := invoke(t, writeTool{s}, map[string]any{"path": "../evil.txt", "content": "x"}); err == nil {
		t.Fatal("writing outside the sandbox should be rejected")
	}
}

func TestBash(t *testing.T) {
	s, _ := newSet(t)
	out, err := invoke(t, bashTool{s}, map[string]any{"command": "echo hello"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "hello") {
		t.Fatalf("bash echo = %q", out)
	}
	out, err = invoke(t, bashTool{s}, map[string]any{"command": "exit 7"})
	if err != nil {
		t.Fatalf("non-zero exit should not be a tool error: %v", err)
	}
	if !strings.Contains(out, "exit status 7") {
		t.Fatalf("bash exit code not reported: %q", out)
	}
	if _, err := invoke(t, bashTool{s}, map[string]any{"command": ""}); err == nil {
		t.Fatal("empty command should error")
	}
}

func TestToolsExposesFullSet(t *testing.T) {
	s, _ := newSet(t)
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

// TestEditContractProperty pins the edit tool's single-match rule: for content
// containing the marker exactly k times, the edit succeeds if and only if k == 1,
// and on success replaces precisely that one occurrence.
func TestEditContractProperty(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		k := rapid.IntRange(0, 4).Draw(rt, "occurrences")
		sep := rapid.StringMatching(`[a-z ]{1,4}`).Draw(rt, "sep")
		parts := make([]string, k)
		for i := range parts {
			parts[i] = "MARK"
		}
		content := "head " + strings.Join(parts, sep) + " tail"

		s, sb := newSet(t)
		seed(t, sb, "f.txt", content)
		_, err := invoke(t, editTool{s}, map[string]any{"path": "f.txt", "old": "MARK", "new": "DONE"})

		switch {
		case k == 1:
			if err != nil {
				rt.Fatalf("k=1 should succeed: %v", err)
			}
			got, _ := invoke(t, readTool{s}, map[string]any{"path": "f.txt"})
			if strings.Contains(got, "MARK") || strings.Count(got, "DONE") != 1 {
				rt.Fatalf("k=1 did not replace exactly one: %q", got)
			}
		case err == nil:
			rt.Fatalf("k=%d should fail (0 or ambiguous)", k)
		}
	})
}

func TestResultSummarizers(t *testing.T) {
	long := strings.Repeat("line\n", 50)
	cases := []struct {
		name string
		tool interface {
			SummarizeResult(json.RawMessage, string) string
		}
		input  string
		result string
		want   []string
	}{
		{"bash exit 0", bashTool{}, `{"command":"go test ./..."}`, long, []string{`"go test ./..."`, "exit 0", "50 lines"}},
		{"bash nonzero", bashTool{}, `{"command":"go build"}`, "boom\n[exit status 2]", []string{"exit status 2"}},
		{"read", readTool{}, `{"path":"a/b.go"}`, long, []string{"a/b.go", "50 lines"}},
		{"glob", globTool{}, `{"pattern":"**/*.go"}`, long, []string{`"**/*.go"`, "50 files"}},
		{"grep", grepTool{}, `{"pattern":"TODO"}`, long, []string{`"TODO"`, "50 matches"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.tool.SummarizeResult(json.RawMessage(tc.input), tc.result)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Fatalf("summary %q missing %q", got, w)
				}
			}
		})
	}
}

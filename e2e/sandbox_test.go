package e2e

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestSandboxPathJailReadEscape scripts the model to read a host file outside the
// workspace by traversal and by absolute path. The read tool must refuse both, the
// refusal must come back as a tool error (so the model, and the record, see it), and the
// run must stay contained and still verify. This proves the path jail at runtime through
// the shipped binary, not only in a unit test.
func TestSandboxPathJailReadEscape(t *testing.T) {
	// The model tries an absolute-path and two traversal escapes, then finishes. Each
	// escape that resolves outside the workspace must be actively denied by the jail,
	// and none may return host file contents. hostSecretPath is an absolute path (drive
	// letter on Windows, / on Unix), so it is a true out-of-jail target on every OS.
	replies := []oaiReply{
		toolCall("r1", "read", `{"path":"`+jsonEscape(hostSecretPath())+`"}`),
		toolCall("r2", "read", `{"path":"../../../../../../etc/passwd"}`),
		toolCall("r3", "read", `{"path":"..\\..\\..\\..\\Windows\\System32\\drivers\\etc\\hosts"}`),
		finalText("I could not read outside the workspace."),
	}
	fake := newFakeOpenAIQueue(t, replies...)
	in := newInstance(t).withModel(fake)

	res := in.run("-no-learn", "goal", "read a host file")
	requireExit(t, res, 0, "contained goal")

	// Collect every tool result fed back (each lands as a tool-role message).
	var toolResults []string
	for i := 1; i < fake.count(); i++ {
		for _, m := range fake.request(t, i).Messages {
			if m.Role == "tool" {
				toolResults = append(toolResults, m.Content)
			}
		}
	}
	if len(toolResults) == 0 {
		t.Fatal("no tool results observed; the escapes did not execute")
	}

	// The jail must have actively fired at least once (an absolute out-of-jail path is
	// denied, not silently missed), and no result may leak host contents. Asserting the
	// active denial keeps this from passing vacuously on a path that merely collapsed
	// back inside the workspace and returned "not found".
	sawDenied := false
	for i, tr := range toolResults {
		low := strings.ToLower(tr)
		if strings.Contains(low, "sandbox_denied") || strings.Contains(low, "denied by the sandbox") {
			sawDenied = true
		}
		if strings.Contains(low, "root:x:0:0") || strings.Contains(low, "root:x:0") {
			t.Fatalf("tool result %d leaked /etc/passwd contents: %q", i, tr)
		}
	}
	if !sawDenied {
		t.Fatalf("no escape was actively denied by the sandbox; results were: %q", toolResults)
	}

	// The run stayed governed and verifiable despite the escape attempts.
	requireExit(t, in.verify(in.runID(res)), 0, "spine verify after contained escapes")
}

// TestSandboxPathJailWriteEscape scripts a write to a path that traverses out of the
// workspace. The file must NOT appear at the escape target, and the workspace stays the
// only place the run can write.
func TestSandboxPathJailWriteEscape(t *testing.T) {
	in := newInstance(t)
	// A sentinel next to (but outside) this instance's workspace that a correct jail
	// can never reach. It sits one level up, in the instance root.
	sentinel := filepath.Join(filepath.Dir(in.workspace), "escaped.txt")
	rel, err := filepath.Rel(in.workspace, sentinel)
	if err != nil {
		rel = "../escaped.txt"
	}

	fake := newFakeOpenAIQueue(
		t,
		toolCall("w1", "write", `{"path":"`+jsonEscape(filepath.ToSlash(rel))+`","content":"pwned"}`),
		finalText("done"),
	)
	in.withModel(fake)

	res := in.run("-no-learn", "goal", "write outside the sandbox")
	requireExit(t, res, 0, "goal")

	if _, err := os.Stat(sentinel); err == nil {
		t.Fatalf("write escaped the sandbox: file created at %s", sentinel)
	}
}

// hostSecretPath returns an absolute host path a contained run must not be able to read.
func hostSecretPath() string {
	if runtime.GOOS == "windows" {
		return `C:\Windows\win.ini`
	}
	return "/etc/passwd"
}

// jsonEscape escapes a path for embedding inside a JSON string literal (backslashes and
// quotes), so a Windows path in a scripted tool call is valid JSON.
func jsonEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}

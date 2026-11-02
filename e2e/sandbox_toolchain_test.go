package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestSandboxGoToolchainRuns scripts the model to run `go env GOCACHE` and `go test` on a
// module in the workspace through the shipped binary's default confinement. The default
// host is read-only, so the build cache's default location under the home directory could
// not be written and a model could never run a test suite. On Windows the child's temp
// directory was also the workspace root, which made Go ignore the module's go.mod. The tool
// results fed back to the model are the evidence: the cache is in the sandbox's scratch
// directory and the tests pass.
func TestSandboxGoToolchainRuns(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain on PATH")
	}

	fake := newFakeOpenAIQueue(
		t,
		toolCall("g1", "bash", `{"command":"go env GOCACHE"}`),
		toolCall("g2", "bash", `{"command":"go test ./..."}`),
		finalText("done"),
	)
	in := newInstance(t).withModel(fake)
	module := map[string]string{
		"go.mod":       "module example.com/tiny\n\ngo 1.21\n",
		"tiny.go":      "package tiny\n\nfunc Add(a, b int) int { return a + b }\n",
		"tiny_test.go": "package tiny\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(1, 2) != 3 {\n\t\tt.Fatal(\"bad sum\")\n\t}\n}\n",
	}
	for name, body := range module {
		if err := os.WriteFile(filepath.Join(in.workspace, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	res := in.run("-no-learn", "goal", "run the tests")
	requireExit(t, res, 0, "goal")

	var toolResults []string
	for i := 1; i < fake.count(); i++ {
		for _, m := range fake.request(t, i).Messages {
			if m.Role == "tool" {
				toolResults = append(toolResults, m.Content)
			}
		}
	}
	all := strings.Join(toolResults, "\n")
	if !strings.Contains(all, "flynn-scratch-") {
		t.Errorf("go env GOCACHE did not name the sandbox scratch directory:\n%s", all)
	}
	if strings.Contains(all, "ignoring go.mod in system temp root") {
		t.Errorf("Go treated the workspace as the system temp root:\n%s", all)
	}
	if !strings.Contains(all, "ok  \texample.com/tiny") {
		t.Errorf("go test did not pass inside the confinement:\n%s", all)
	}
}

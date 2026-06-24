package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/llm/llmtest"
)

// TestRunMissionWritesFileThroughSandbox is the full-binary proof: runMission
// assembles the real runtime, sandbox, and toolset, and a scripted model drives a
// goal that writes a file through the sandboxed write tool, then converges with a
// summary. No network: the model is a fake.
func TestRunMissionWritesFileThroughSandbox(t *testing.T) {
	dir := t.TempDir()
	model := llmtest.NewScripted(
		llmtest.CallTool("c1", "write", json.RawMessage(`{"path":"hello.txt","content":"hi from flynn"}`)),
		llmtest.SayText("Created hello.txt with a greeting."),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out bytes.Buffer

	result, err := runMission(ctx, &out, model, dir, "create hello.txt with a greeting")
	if err != nil {
		t.Fatalf("runMission: %v", err)
	}

	// The file was actually written, through the sandboxed tool.
	b, err := os.ReadFile(filepath.Join(dir, "hello.txt"))
	if err != nil || string(b) != "hi from flynn" {
		t.Fatalf("file not written through the sandbox: err=%v content=%q", err, b)
	}
	// The final result is the model's summary.
	if !strings.Contains(result, "Created hello.txt") {
		t.Fatalf("final result = %q", result)
	}
	// Progress showed the tool action.
	if !strings.Contains(out.String(), "write") {
		t.Fatalf("progress did not show the tool action:\n%s", out.String())
	}
}

// TestRunMissionRejectsSandboxEscape confirms the wired path is confined: a tool
// call that tries to write outside the working directory is denied, so the agent
// cannot touch the host beyond its sandbox even end to end.
func TestRunMissionRejectsSandboxEscape(t *testing.T) {
	dir := t.TempDir()
	model := llmtest.NewScripted(
		llmtest.CallTool("c1", "write", json.RawMessage(`{"path":"../escape.txt","content":"nope"}`)),
		llmtest.SayText("done"),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out bytes.Buffer

	if _, err := runMission(ctx, &out, model, dir, "try to escape"); err != nil {
		t.Fatalf("runMission: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "escape.txt")); !os.IsNotExist(err) {
		t.Fatal("a tool wrote outside the sandbox working directory")
	}
}

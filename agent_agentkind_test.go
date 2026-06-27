package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ionalpha/flynn/archetype"
	"github.com/ionalpha/flynn/llm/llmtest"
	"github.com/ionalpha/flynn/resource"
)

// storeAgent registers the Agent kind, stores spec under name, and returns the
// decoded spec read back from the store, so the test exercises the full
// store -> load -> run-as loop rather than a hand-built spec.
func storeAgent(t *testing.T, name string, spec archetype.Spec) archetype.Spec {
	t.Helper()
	reg := resource.NewRegistry()
	if err := resource.RegisterCoreKinds(reg); err != nil {
		t.Fatal(err)
	}
	if err := archetype.RegisterKind(reg); err != nil {
		t.Fatal(err)
	}
	store := resource.NewMemory(reg)
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(context.Background(), resource.Resource{
		APIVersion: archetype.GroupVersion, Kind: archetype.Kind, Name: name, Spec: raw,
	}); err != nil {
		t.Fatalf("store agent: %v", err)
	}
	got, err := store.Get(context.Background(), archetype.Kind, resource.Scope{}, name)
	if err != nil {
		t.Fatalf("load agent: %v", err)
	}
	decoded, err := archetype.DecodeSpec(got)
	if err != nil {
		t.Fatalf("decode agent: %v", err)
	}
	return decoded
}

// TestAgentKindGovernsTheRun is the e2e proof that an Agent resource is the
// least-privilege boundary: a "researcher" archetype that does not list the write
// capability cannot write a file, even though the host offers the write tool and
// the model tries to use it. The write is refused at the dispatch waist by the
// grant derived from the Agent's capabilities, and the run still completes.
func TestAgentKindGovernsTheRun(t *testing.T) {
	dir := t.TempDir()
	researcher := storeAgent(t, "researcher", archetype.Spec{
		System:       "You research and report. You never modify files.",
		Capabilities: []string{"read", "glob", "grep"}, // deliberately no write/bash
	})

	model := llmtest.NewScripted(
		llmtest.CallTool("c1", "write", json.RawMessage(`{"path":"out.txt","content":"should be denied"}`)),
		llmtest.SayText("I cannot modify files, so I reported instead."),
	)
	a := New(Config{WorkDir: dir, Agent: &researcher})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := a.runGoal(ctx, model, "write out.txt"); err != nil {
		t.Fatalf("runGoal: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "out.txt")); !os.IsNotExist(err) {
		t.Fatalf("the researcher archetype lacks write capability; the file must not exist (stat err=%v)", err)
	}
	if model.Calls() != 2 {
		t.Fatalf("model called %d times, want 2 (denied write result fed back, then final answer)", model.Calls())
	}
}

// TestAgentKindAllowsDeclaredCapability is the positive control: a "builder"
// archetype that lists write can write through the same path.
func TestAgentKindAllowsDeclaredCapability(t *testing.T) {
	dir := t.TempDir()
	builder := storeAgent(t, "builder", archetype.Spec{
		System:       "You build and modify files as asked.",
		Capabilities: []string{"read", "write"},
	})

	model := llmtest.NewScripted(
		llmtest.CallTool("c1", "write", json.RawMessage(`{"path":"out.txt","content":"built"}`)),
		llmtest.SayText("Wrote out.txt."),
	)
	a := New(Config{WorkDir: dir, Agent: &builder})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := a.runGoal(ctx, model, "write out.txt"); err != nil {
		t.Fatalf("runGoal: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "out.txt")); err != nil || string(b) != "built" {
		t.Fatalf("builder archetype should have written the file: err=%v content=%q", err, b)
	}
}

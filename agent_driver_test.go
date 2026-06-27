package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/llm/llmtest"
)

// TestDefaultDriverRunsTheToolLoop is the behaviour-identical proof: with no driver
// named, a goal runs the general-purpose tool loop, calling a tool and converging,
// exactly as before the loop became pluggable.
func TestDefaultDriverRunsTheToolLoop(t *testing.T) {
	dir := t.TempDir()
	model := llmtest.NewScripted(
		llmtest.CallTool("c1", "write", json.RawMessage(`{"path":"out.txt","content":"hi"}`)),
		llmtest.SayText("Done."),
	)
	a := New(Config{WorkDir: dir}) // Driver == "" -> general-software

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := a.runGoal(ctx, model, "write out.txt"); err != nil {
		t.Fatalf("runGoal: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "out.txt")); err != nil || string(b) != "hi" {
		t.Fatalf("default driver did not run the tool loop: err=%v content=%q", err, b)
	}
}

// TestSingleShotDriverAnswersInOneTurn proves the Driver boundary: selecting the
// single-shot driver swaps the loop shape. The model is given a tool call AND a
// follow-up answer, but the single-shot loop never offers tools and converges on
// the first turn, so it returns the model's first-turn text and writes no file.
func TestSingleShotDriverAnswersInOneTurn(t *testing.T) {
	dir := t.TempDir()
	// The single-shot loop offers no tools, so the model's first turn is its answer.
	model := llmtest.NewScripted(llmtest.SayText("The answer is 42."))
	a := New(Config{WorkDir: dir, Driver: "single-shot"})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := a.runGoal(ctx, model, "state the answer")
	if err != nil {
		t.Fatalf("runGoal: %v", err)
	}
	if !strings.Contains(result, "42") {
		t.Fatalf("single-shot result = %q, want the model's answer", result)
	}
	if model.Calls() != 1 {
		t.Fatalf("single-shot called the model %d times, want exactly 1 turn", model.Calls())
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("single-shot must not write files, found %d entries", len(entries))
	}
}

// TestUnknownDriverFailsClosed proves a misnamed driver stops the run at assembly
// rather than silently running the wrong loop.
func TestUnknownDriverFailsClosed(t *testing.T) {
	a := New(Config{WorkDir: t.TempDir(), Driver: "no-such-driver"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := a.runGoal(ctx, llmtest.NewScripted(llmtest.SayText("hi")), "do something")
	if err == nil {
		t.Fatal("an unknown driver must fail closed")
	}
	if !strings.Contains(err.Error(), "no-such-driver") {
		t.Fatalf("error should name the unknown driver, got: %v", err)
	}
}

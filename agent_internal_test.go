package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/llm/llmtest"
)

// failingModel always returns a terminal fault: the agent must report the run as
// failed, never narrate success.
type failingModel struct{}

func (failingModel) Generate(context.Context, llm.Request) (llm.Response, error) {
	return llm.Response{}, fault.New(fault.Terminal, "test_model_down", "the model is unavailable")
}

// blockingModel parks every Generate call until the context is cancelled, so a test
// can drive the run into flight and then cancel it.
type blockingModel struct{}

func (blockingModel) Generate(ctx context.Context, _ llm.Request) (llm.Response, error) {
	<-ctx.Done()
	return llm.Response{}, ctx.Err()
}

// TestAgentRunsGoalEndToEnd is the embedding proof: an Agent built from Config
// drives a real goal to completion through the assembled runtime and sandboxed
// toolset, writing a file through the confined write tool and returning the model's
// final answer. The model is a scripted fake, so the whole library entry runs with
// no network and no API key, the same path Goal takes against a real provider.
func TestAgentRunsGoalEndToEnd(t *testing.T) {
	dir := t.TempDir()
	model := llmtest.NewScripted(
		llmtest.CallTool("c1", "write", json.RawMessage(`{"path":"out.txt","content":"hi from flynn"}`)),
		llmtest.SayText("Created out.txt."),
	)
	a := New(Config{WorkDir: dir})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	result, err := a.runGoal(ctx, model, "create out.txt with a greeting")
	if err != nil {
		t.Fatalf("runGoal: %v", err)
	}
	if !strings.Contains(result, "Created out.txt") {
		t.Fatalf("final result = %q", result)
	}
	b, err := os.ReadFile(filepath.Join(dir, "out.txt"))
	if err != nil || string(b) != "hi from flynn" {
		t.Fatalf("file not written through the sandbox: err=%v content=%q", err, b)
	}
}

// TestAgentSurfacesAFailedRun is the clean-degradation chaos check: when the model
// is unavailable, the run is reported as a failure rather than returning a hollow
// success or hanging. The agent's whole point is not narrating work it did not do.
func TestAgentSurfacesAFailedRun(t *testing.T) {
	a := New(Config{WorkDir: t.TempDir()})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type outcome struct {
		result string
		err    error
	}
	ch := make(chan outcome, 1)
	go func() {
		r, err := a.runGoal(ctx, failingModel{}, "do something")
		ch <- outcome{r, err}
	}()

	// A terminal model failure is not retryable: the run must surface in seconds,
	// not after the worker burns its whole retry budget (which would be tens of
	// seconds). The window also guards against a regression to retry-on-terminal.
	select {
	case got := <-ch:
		if got.err == nil {
			t.Fatalf("a failing model must surface an error; got result %q", got.result)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("a terminal model failure did not surface within 8s; it must fail fast, not retry")
	}
}

// TestAgentHonoursCancellationMidRun is the no-hang chaos check: a run in flight,
// cancelled, returns promptly with an error instead of deadlocking. The blocking
// model holds the turn open until the context is cancelled.
func TestAgentHonoursCancellationMidRun(t *testing.T) {
	a := New(Config{WorkDir: t.TempDir()})
	ctx, cancel := context.WithCancel(context.Background())

	type outcome struct {
		result string
		err    error
	}
	ch := make(chan outcome, 1)
	go func() {
		r, err := a.runGoal(ctx, blockingModel{}, "wait then stop")
		ch <- outcome{r, err}
	}()

	// Let the run reach the blocking model call, then cancel.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case got := <-ch:
		if got.err == nil {
			t.Fatalf("cancelled run must return an error; got result %q", got.result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runGoal did not return within 5s of cancellation (it hung)")
	}
}

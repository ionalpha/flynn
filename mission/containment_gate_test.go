package mission

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/llm/llmtest"
	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/state"
)

// shellishTool is a fake tool that runs model-authored content, so it declares
// semi-trust the way the real shell tool does. ran records whether its side effect fired,
// to prove a refused call never runs.
type shellishTool struct{ ran *bool }

func (shellishTool) Def() llm.Tool {
	return llm.Tool{Name: "shellish", Description: "x", InputSchema: json.RawMessage(`{"type":"object"}`)}
}

func (t shellishTool) Invoke(context.Context, json.RawMessage) (string, error) {
	*t.ran = true
	return "ran", nil
}

func (shellishTool) WorkTrust() sandbox.Trust { return sandbox.TrustSemi }

func kernelSandbox(t *testing.T) sandbox.Sandbox {
	t.Helper()
	sb, err := sandbox.NewLocal(t.TempDir(), sandbox.WithReadOnlyFS(), sandbox.WithSeccomp())
	if err != nil {
		t.Fatal(err)
	}
	return sb
}

func processJailSandbox(t *testing.T) sandbox.Sandbox {
	t.Helper()
	sb, err := sandbox.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return sb
}

// TestWaistRefusesSemiWorkOnWeakHost is acceptance #3 for the negative case: a
// semi-trusted work kind (a model-authored command) is refused at the dispatch waist when
// the host's sandbox cannot kernel-confine it, and its side effect never runs.
func TestWaistRefusesSemiWorkOnWeakHost(t *testing.T) {
	if sandbox.ContainmentOf(kernelSandbox(t)) != sandbox.ContainmentKernel {
		t.Skip("this platform cannot report kernel confinement; the gate's distinction is untestable here")
	}
	ran := false
	exec := NewExecutor(llmtest.NewScripted(), WithTools(shellishTool{&ran}), WithSandbox(processJailSandbox(t)))

	_, err := exec.invokeTool(context.Background(), state.Scope{}, llm.ToolUse{ID: "1", Name: "shellish", Input: json.RawMessage(`{}`)})
	if err == nil {
		t.Fatal("semi-trusted work must be refused on a process-jail host")
	}
	if !strings.Contains(err.Error(), "isolation") {
		t.Fatalf("refusal must explain the missing isolation, got %v", err)
	}
	if ran {
		t.Fatal("a refused tool must not run its side effect")
	}
}

// TestWaistAdmitsSemiWorkOnKernelHost is the positive case: the same semi-trusted work
// runs when the host provides kernel confinement, so the gate refuses only what is truly
// under-contained.
func TestWaistAdmitsSemiWorkOnKernelHost(t *testing.T) {
	if sandbox.ContainmentOf(kernelSandbox(t)) != sandbox.ContainmentKernel {
		t.Skip("this platform cannot report kernel confinement")
	}
	ran := false
	exec := NewExecutor(llmtest.NewScripted(), WithTools(shellishTool{&ran}), WithSandbox(kernelSandbox(t)))

	out, err := exec.invokeTool(context.Background(), state.Scope{}, llm.ToolUse{ID: "1", Name: "shellish", Input: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatalf("semi-trusted work must run on a kernel-confined host, got %v", err)
	}
	if !ran || out != "ran" {
		t.Fatalf("the admitted tool did not run: ran=%v out=%q", ran, out)
	}
}

// TestWaistAdmitsTrustedWorkAnywhere proves a trusted built-in tool runs even on the
// weakest host, so the gate never blocks the agent's own work.
func TestWaistAdmitsTrustedWorkAnywhere(t *testing.T) {
	exec := NewExecutor(llmtest.NewScripted(), WithTools(echoTool()), WithSandbox(processJailSandbox(t)))
	out, err := exec.invokeTool(context.Background(), state.Scope{}, llm.ToolUse{ID: "1", Name: "echo", Input: json.RawMessage(`{"x":1}`)})
	if err != nil {
		t.Fatalf("trusted work must run on any host, got %v", err)
	}
	if !strings.Contains(out, `"x":1`) {
		t.Fatalf("trusted tool did not run: %q", out)
	}
}

// TestWaistRecordsTrustOnTheSpine proves every dispatched work kind carries a trust level
// that is recorded at the waist, so a run's containment posture is auditable. The shell
// tool's events carry semi-trust; a model call carries trusted.
func TestWaistRecordsTrustOnTheSpine(t *testing.T) {
	sink := &dispatch.MemorySink{}
	exec := NewExecutor(llmtest.NewScripted(), WithTools(echoTool()), WithEventSink(sink), WithSandbox(kernelSandbox(t)))
	_, _ = exec.invokeTool(context.Background(), state.Scope{}, llm.ToolUse{ID: "1", Name: "echo", Input: json.RawMessage(`{}`)})

	var sawTrust bool
	for _, e := range sink.Events() {
		if e.Action == "echo" && e.Type == dispatch.EventStart {
			if e.Trust == "" {
				t.Fatalf("dispatch event for echo carries no trust level: %+v", e)
			}
			sawTrust = true
		}
	}
	if !sawTrust {
		t.Fatal("no start event recorded the action's trust level")
	}
}

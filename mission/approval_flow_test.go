package mission

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ionalpha/flynn/approval"
	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/llm/llmtest"
)

// captureSink records every approval Decision the gate makes, so a test can assert what
// the waist actually authorized (granted, and the envelope it was bound to).
type captureSink struct {
	mu   sync.Mutex
	decs []approval.Decision
}

func (c *captureSink) Record(_ context.Context, d approval.Decision) error {
	c.mu.Lock()
	c.decs = append(c.decs, d)
	c.mu.Unlock()
	return nil
}

func (c *captureSink) granted() (approval.Decision, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, d := range c.decs {
		if d.Granted {
			return d, true
		}
	}
	return approval.Decision{}, false
}

// promptFunc adapts a function to the ApprovalPrompter port.
type promptFunc func(ctx context.Context, req ApprovalRequest) (ApprovalDecision, error)

func (f promptFunc) Prompt(ctx context.Context, req ApprovalRequest) (ApprovalDecision, error) {
	return f(ctx, req)
}

// approvalExec builds an executor whose "echo" tool needs one approval, wired to prompter
// for the decision. It returns the executor, the counting tool's call count pointer, the
// gate's decision sink, and the scripted model, so a test drives it and inspects both the
// tool side effect and what was authorized. The verifier and the executor share clk, so a
// minted approval's window is checked against the same time it was stamped from.
func approvalExec(t *testing.T, clk clock.Clock, prompter ApprovalPrompter, grace time.Duration) (*Executor, *int, *captureSink, *llmtest.ScriptedModel) {
	t.Helper()
	const host = "host-A"
	signer, pub, err := approval.GenerateEd25519Signer("approver-1", rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	kr := approval.NewKeyring()
	if err := kr.Add("approver-1", pub); err != nil {
		t.Fatal(err)
	}
	v := approval.NewVerifier(kr, approval.NewMemStore(), approval.WithClock(clk), approval.WithHost(host))
	sink := &captureSink{}
	gate := approval.NewGate(approval.Requirements{"echo": 1}, v, approval.WithGateHost(host), approval.WithSink(sink))

	calls := 0
	tool := Func(echoDef, func(_ context.Context, input json.RawMessage) (string, error) {
		calls++
		return string(input), nil
	})
	model := llmtest.NewScripted(
		llmtest.CallTool("t1", "echo", json.RawMessage(`{"x":1}`)),
		llmtest.SayText("ok"),
	)
	opts := []Option{
		WithTools(tool),
		WithApproval(gate),
		WithApprovalPrompter(prompter, signer, host),
		WithApprovalClock(clk),
	}
	if grace > 0 {
		opts = append(opts, WithApprovalGrace(grace))
	}
	return NewExecutor(model, opts...), &calls, sink, model
}

// lastToolResult returns the tool result the model saw on its second request, so a test
// asserts what the approval outcome surfaced to the run.
func lastToolResult(t *testing.T, model *llmtest.ScriptedModel) *llm.ToolResult {
	t.Helper()
	reqs := model.Requests()
	if len(reqs) < 2 {
		t.Fatalf("model was called %d times, want at least 2", len(reqs))
	}
	for _, m := range reqs[1].Messages {
		for _, b := range m.Blocks {
			if b.Kind == llm.KindToolResult {
				return b.ToolResult
			}
		}
	}
	t.Fatal("no tool result surfaced to the model")
	return nil
}

// TestApprovalAllowAdmitsAction proves an allowed action runs: the waist pauses it for
// approval, the prompter allows, the executor mints and presents a single-use approval,
// and the retry admits the action so its side effect happens once.
func TestApprovalAllowAdmitsAction(t *testing.T) {
	clk := clock.NewManual(time.Unix(1000, 0))
	prompter := promptFunc(func(_ context.Context, _ ApprovalRequest) (ApprovalDecision, error) {
		return ApprovalDecision{Allow: true}, nil
	})
	exec, calls, sink, model := approvalExec(t, clk, prompter, 0)
	driveToDone(t, exec, 5)

	if *calls != 1 {
		t.Fatalf("approved tool ran %d times, want 1", *calls)
	}
	if r := lastToolResult(t, model); r.IsError {
		t.Fatalf("approved tool surfaced an error result: %q", r.Content)
	}
	if _, ok := sink.granted(); !ok {
		t.Fatal("no grant was recorded for the approved action")
	}
}

// TestApprovalScopeBindsGrant proves an allow scoped to a target binds the minted
// approval to exactly that target: the granted decision's envelope carries the scope as
// its detail, so the authorization cannot be widened to another target.
func TestApprovalScopeBindsGrant(t *testing.T) {
	clk := clock.NewManual(time.Unix(1000, 0))
	prompter := promptFunc(func(_ context.Context, _ ApprovalRequest) (ApprovalDecision, error) {
		return ApprovalDecision{Allow: true, Scope: "workspace/reports"}, nil
	})
	exec, calls, sink, _ := approvalExec(t, clk, prompter, 0)
	driveToDone(t, exec, 5)

	if *calls != 1 {
		t.Fatalf("approved tool ran %d times, want 1", *calls)
	}
	dec, ok := sink.granted()
	if !ok {
		t.Fatal("no grant recorded")
	}
	if dec.Envelope.Detail != "workspace/reports" {
		t.Fatalf("granted detail = %q, want the scoped target", dec.Envelope.Detail)
	}
}

// TestApprovalDenyFeedsBackToRun proves a denial with feedback never runs the action and
// surfaces the reason to the model as an error result it can adapt to.
func TestApprovalDenyFeedsBackToRun(t *testing.T) {
	clk := clock.NewManual(time.Unix(1000, 0))
	prompter := promptFunc(func(_ context.Context, _ ApprovalRequest) (ApprovalDecision, error) {
		return ApprovalDecision{Allow: false, Feedback: "not this path"}, nil
	})
	exec, calls, _, model := approvalExec(t, clk, prompter, 0)
	driveToDone(t, exec, 5)

	if *calls != 0 {
		t.Fatalf("denied tool ran %d times, want 0", *calls)
	}
	r := lastToolResult(t, model)
	if !r.IsError || !strings.Contains(r.Content, "not this path") {
		t.Fatalf("denial feedback not surfaced to the run: %+v", r)
	}
}

// TestApprovalGraceExpiryDeclines proves a prompt the human never answers auto-declines
// once the grace period elapses: the action stays refused (fail-closed) and never runs.
func TestApprovalGraceExpiryDeclines(t *testing.T) {
	clk := clock.NewManual(time.Unix(1000, 0))
	prompter := promptFunc(func(ctx context.Context, _ ApprovalRequest) (ApprovalDecision, error) {
		<-ctx.Done() // never decide; wait for the grace period to expire
		return ApprovalDecision{}, ctx.Err()
	})
	exec, calls, _, model := approvalExec(t, clk, prompter, 50*time.Millisecond)
	driveToDone(t, exec, 5)

	if *calls != 0 {
		t.Fatalf("action ran %d times after grace expiry, want 0", *calls)
	}
	r := lastToolResult(t, model)
	if !r.IsError || !strings.Contains(r.Content, "grace period") {
		t.Fatalf("grace-period decline not surfaced to the run: %+v", r)
	}
}

// TestApprovalNilPrompterUnchanged proves that without a prompter a NeedsApproval
// rejection surfaces to the model unchanged, so the standalone agent is zero-config.
func TestApprovalNilPrompterUnchanged(t *testing.T) {
	clk := clock.NewManual(time.Unix(1000, 0))
	exec, calls, _, model := approvalExec(t, clk, nil, 0)
	driveToDone(t, exec, 5)

	if *calls != 0 {
		t.Fatalf("action ran %d times with no prompter, want 0 (still gated)", *calls)
	}
	if r := lastToolResult(t, model); !r.IsError {
		t.Fatalf("needs-approval rejection was not surfaced as an error: %+v", r)
	}
}

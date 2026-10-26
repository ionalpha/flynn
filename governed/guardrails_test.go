package governed

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ionalpha/flynn/allowance"
	"github.com/ionalpha/flynn/brakes"
	budgetpkg "github.com/ionalpha/flynn/budget"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/driver"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/llm/llmtest"
	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/spine"
)

// callingOnce is a script whose first turn calls tool with input and reports tokens spent,
// so a ceiling of one token is exhausted before the call is dispatched.
func callingOnce(tool, input string) *llmtest.ScriptedModel {
	call := llmtest.CallTool("c1", tool, json.RawMessage(input))
	call.Usage = llm.Usage{InputTokens: 10}
	return llmtest.NewScripted(call, llmtest.SayText("done"))
}

const writeInput = `{"path":"hello.txt","content":"hi"}`

// startWithoutPrepare drives the goal the way a host that calls Submit does: it starts the
// runtime and submits, and never calls [Run.Prepare]. The run's guards must not depend on
// the caller having made a call that [Run.Do] happens to make.
func startWithoutPrepare(t *testing.T, run *Run, objective string) {
	t.Helper()
	ctx, cancel := context.WithCancel(ctxT(t))
	defer cancel()
	done := make(chan struct{})
	go func() { _ = run.rt.Start(ctx); close(done) }()
	if _, err := run.Submit(ctx, goal.Spec{Objective: objective}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	_, _ = run.p.sess.Wait(ctx)
	cancel()
	<-done
}

// refusalCodes lists the codes of the actions the run's waist rejected, in order.
func refusalCodes(t *testing.T, log spine.Log, stream, action string) []string {
	t.Helper()
	events, err := log.Read(context.Background(), spine.Query{Stream: stream})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var codes []string
	for _, e := range events {
		if e.Type == dispatch.EventRejected && e.Payload["action"] == action {
			code, _ := e.Payload["error_code"].(string)
			codes = append(codes, code)
		}
	}
	return codes
}

// TestBudgetCeilingBindsOnEveryPath: WithBudget is a guard the host asked for, and Assemble
// opens it, so a goal started with Submit (which skips Prepare) is refused once the ceiling
// is spent. Without the ceiling the same script writes the file.
func TestBudgetCeilingBindsOnEveryPath(t *testing.T) {
	tests := []struct {
		name      string
		opts      []Option
		wantWrite bool
	}{
		{"no ceiling", nil, true},
		{"one token ceiling", []Option{WithBudget(budgetpkg.Limits{Tokens: 1})}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			b, _ := memBackends(t)
			run, err := Assemble(ctxT(t), callingOnce("write", writeInput), b,
				append([]Option{WithWorkDir(dir)}, tt.opts...)...)
			if err != nil {
				t.Fatalf("assemble: %v", err)
			}
			defer func() { _ = run.Close() }()

			startWithoutPrepare(t, run, "write hello.txt")

			_, statErr := os.Stat(filepath.Join(dir, "hello.txt"))
			if wrote := statErr == nil; wrote != tt.wantWrite {
				t.Fatalf("file written = %v, want %v", wrote, tt.wantWrite)
			}
		})
	}
}

// TestPostureReportsWhatIsEnforced: a guard the record claims is a guard the run has. An
// external loop runs its own tools beyond the approval gate, so it states none; the limits
// of the brakes and the ceiling are the ones in force, not the ones asked for.
func TestPostureReportsWhatIsEnforced(t *testing.T) {
	drv, err := driver.Default().Resolve(driver.NameDefault)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	t.Run("external loop states no approval gate", func(t *testing.T) {
		b, _ := memBackends(t)
		run, err := Assemble(ctxT(t), nil, b, WithWorkDir(t.TempDir()), WithExternalAgent(ExternalAgent{Model: "x", Driver: drv}))
		if err != nil {
			t.Fatalf("assemble: %v", err)
		}
		defer func() { _ = run.Close() }()

		p := run.Posture()
		if p.Approval || len(p.ApprovalActions) != 0 {
			t.Fatalf("posture = %+v, want no approval claimed for a loop that bypasses the gate", p)
		}
		ev := postureEvents(t, b.Log, run.ID())
		if len(ev) != 1 {
			t.Fatalf("recorded %d posture statements, want 1", len(ev))
		}
		if got, _ := ev[0].Payload["approval"].(bool); got {
			t.Errorf("the record claims approval for an external loop: %v", ev[0].Payload)
		}
	})

	t.Run("limits in force", func(t *testing.T) {
		b, _ := memBackends(t)
		limits := brakes.Limits{MaxActions: 7, Window: 3 * time.Second}
		run, err := Assemble(ctxT(t), say("x"), b, WithWorkDir(t.TempDir()),
			WithBrakeLimits(limits), WithBudget(budgetpkg.Limits{Tokens: 500, Cost: 2.5}))
		if err != nil {
			t.Fatalf("assemble: %v", err)
		}
		defer func() { _ = run.Close() }()

		p := run.Posture()
		if p.BrakeLimits.MaxActions != 7 || p.BudgetLimits.Tokens != 500 || p.BudgetLimits.Cost != 2.5 {
			t.Fatalf("posture = %+v, want the brake and budget limits asked for", p)
		}
		ev := postureEvents(t, b.Log, run.ID())
		if len(ev) != 1 {
			t.Fatalf("recorded %d posture statements, want 1", len(ev))
		}
		if got, _ := ev[0].Payload["budgetTokens"].(float64); got != 500 {
			t.Errorf("record says budgetTokens = %v, want 500 (payload %v)", ev[0].Payload["budgetTokens"], ev[0].Payload)
		}
		if got, _ := ev[0].Payload["brakeMaxActions"].(float64); got != 7 {
			t.Errorf("record says brakeMaxActions = %v, want 7", ev[0].Payload["brakeMaxActions"])
		}
	})

	t.Run("waived guards state no limits", func(t *testing.T) {
		b, _ := memBackends(t)
		run, err := Assemble(ctxT(t), say("x"), b, WithWorkDir(t.TempDir()),
			Waive(GuardBrakes, "the host halts runs"), Waive(GuardBudget, "the host meters spend"))
		if err != nil {
			t.Fatalf("assemble: %v", err)
		}
		defer func() { _ = run.Close() }()

		if p := run.Posture(); p.BrakeLimits != (brakes.Limits{}) || p.BudgetLimits != (budgetpkg.Limits{}) {
			t.Fatalf("posture = %+v, want no limits stated for guards that are off", p)
		}
	})
}

// TestApprovalPolicyNamesMustBeActionsTheRunOffers: the gate matches an action by its exact
// name, so a misspelled name gates nothing and the run would report an approval gate that
// no action reaches. The run is refused instead, naming what it does offer.
func TestApprovalPolicyNamesMustBeActionsTheRunOffers(t *testing.T) {
	tests := []struct {
		name    string
		actions []string
		ok      bool
	}{
		{"a tool the run offers", []string{"write"}, true},
		{"padding is ignored", []string{" write "}, true},
		{"no policy", nil, true},
		{"a misspelling", []string{"wirte"}, false},
		{"an action no tool provides", []string{"shell"}, false},
		{"one good name and one bad", []string{"write", "shell"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, _ := memBackends(t)
			run, err := Assemble(ctxT(t), say("x"), b, WithWorkDir(t.TempDir()), WithApproval(tt.actions, nil))
			if err == nil {
				_ = run.Close()
			}
			if (err == nil) != tt.ok {
				t.Fatalf("err = %v, want ok=%v", err, tt.ok)
			}
		})
	}
}

// TestGuardsAreConsultedInOrder pins the order the waist applies the guards in, because it
// decides which rule a refusal is attributed to and what each later guard has already
// counted: containment, then budget, then brakes, then approval, then allowance. Each row
// arranges for two guards to refuse the same action and reads which one spoke first.
func TestGuardsAreConsultedInOrder(t *testing.T) {
	inner, err := sandbox.NewLocal(t.TempDir(), sandbox.WithDefaultConfinement())
	if err != nil {
		t.Fatalf("sandbox: %v", err)
	}
	defer func() { _ = inner.Close() }()
	bare := &bareSandbox{Sandbox: inner}

	spent := WithBudget(budgetpkg.Limits{Tokens: 1})
	spentTokens := WithBrakeLimits(brakes.Limits{MaxActions: 1000, Window: time.Minute, MaxTokens: 1})
	tests := []struct {
		name   string
		tool   string
		input  string
		opts   []Option
		hostSb bool
		wantIn string
	}{
		{
			"containment before budget", "bash", `{"command":"true"}`,
			[]Option{WithSandbox(bare), spent, WithApproval([]string{"bash"}, nil)},
			true, "containment_gate",
		},
		{
			"budget before brakes", "write", writeInput,
			[]Option{spent, spentTokens, WithApproval([]string{"write"}, nil)},
			false, "budget_exceeded",
		},
		{
			"brakes before approval", "write", writeInput,
			[]Option{spentTokens, WithApproval([]string{"write"}, nil)},
			false, "run_halted",
		},
		{
			"approval before allowance", "write", writeInput,
			[]Option{WithApproval([]string{"write"}, nil), WithAllowance([]string{"write"})},
			false, "approval_required",
		},
		{
			"allowance alone", "write", writeInput,
			[]Option{WithAllowance([]string{"write"})},
			false, allowance.CodeRequired,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, _ := memBackends(t)
			opts := tt.opts
			if !tt.hostSb {
				opts = append([]Option{WithWorkDir(t.TempDir())}, opts...)
			}
			run, err := Assemble(ctxT(t), callingOnce(tt.tool, tt.input), b, opts...)
			if err != nil {
				t.Fatalf("assemble: %v", err)
			}
			defer func() { _ = run.Close() }()

			startWithoutPrepare(t, run, "call the tool")

			codes := refusalCodes(t, b.Log, run.ID(), tt.tool)
			if len(codes) == 0 || codes[0] != tt.wantIn {
				t.Fatalf("first refusal of %s = %v, want %s", tt.tool, codes, tt.wantIn)
			}
		})
	}
}

// TestAllowanceNamesMustBeActionsTheRunOffers: the allowance gate marks an action by its
// exact name, so a name no tool carries marks nothing. The posture would still say an
// allowance gate is on, so the run is refused instead; a name that matches reports it on.
func TestAllowanceNamesMustBeActionsTheRunOffers(t *testing.T) {
	tests := []struct {
		name    string
		actions []string
		ok      bool
	}{
		{"a tool the run offers", []string{"bash"}, true},
		{"padding is ignored", []string{" bash "}, true},
		{"an action no tool provides", []string{"shell"}, false},
		{"one good name and one bad", []string{"bash", "shell"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, _ := memBackends(t)
			run, err := Assemble(ctxT(t), say("x"), b, WithWorkDir(t.TempDir()), WithAllowance(tt.actions))
			if err == nil {
				defer func() { _ = run.Close() }()
			}
			if (err == nil) != tt.ok {
				t.Fatalf("err = %v, want ok=%v", err, tt.ok)
			}
			if err == nil && !run.Posture().Allowance {
				t.Errorf("posture = %+v, want the allowance gate reported on", run.Posture())
			}
		})
	}
}

// TestResumedRunStatesThePoolItIsChargedAgainst: opening a budget returns the pool that
// already exists, so a resumption that asks for a larger ceiling still runs under the first
// one. The posture, and the record of it, state the ceiling in force and not the one asked for.
func TestResumedRunStatesThePoolItIsChargedAgainst(t *testing.T) {
	b, _ := memBackends(t)
	first, err := Assemble(ctxT(t), say("done"), b, WithWorkDir(t.TempDir()),
		WithBudget(budgetpkg.Limits{Tokens: 100}))
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	defer func() { _ = first.Close() }()

	second, err := Assemble(ctxT(t), say("done"), b, WithWorkDir(t.TempDir()), WithRunID(first.ID()),
		WithBudget(budgetpkg.Limits{Tokens: 5000}))
	if err != nil {
		t.Fatalf("assemble the resumed run: %v", err)
	}
	defer func() { _ = second.Close() }()

	if got := second.Posture().BudgetLimits.Tokens; got != 100 {
		t.Fatalf("posture states a ceiling of %d tokens, want the 100 the pool was opened with", got)
	}
	ev := postureEvents(t, b.Log, first.ID())
	if len(ev) != 2 {
		t.Fatalf("recorded %d posture statements, want 2", len(ev))
	}
	if got, _ := ev[1].Payload["budgetTokens"].(float64); got != 100 {
		t.Errorf("the resumed run's record says budgetTokens = %v, want 100", ev[1].Payload["budgetTokens"])
	}
}

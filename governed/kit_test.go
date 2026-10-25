package governed

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/ionalpha/flynn/brakes"
	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/llm/llmtest"
	"github.com/ionalpha/flynn/mission"
)

// TestKitOptionsAreWorkingTreeOnly: a kit has no goal loop, so an option that only a loop
// can honour is an error rather than a promise the kit would drop, and the one guard a kit
// can waive is recorded like any other.
func TestKitOptionsAreWorkingTreeOnly(t *testing.T) {
	tests := []struct {
		name string
		opts []Option
		ok   bool
	}{
		{"empty workdir", []Option{WithWorkDir("")}, false},
		{"approval", []Option{WithApproval([]string{"bash"}, nil)}, false},
		{"allowance", []Option{WithAllowance([]string{"deploy"})}, false},
		{"budget", nil, true},
		{"brake limits", []Option{WithBrakeLimits(brakes.Limits{MaxActions: 3, Window: 1})}, false},
		{"waive audit", []Option{Waive(GuardAudit, "r")}, false},
		{"waive brakes", []Option{Waive(GuardBrakes, "r")}, true},
		{"planning", []Option{WithPlanning()}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, _ := memBackends(t)
			kit, err := NewKit(ctxT(t), b.Log, append([]Option{WithWorkDir(t.TempDir())}, tt.opts...)...)
			if (err == nil) != tt.ok {
				t.Fatalf("err = %v, want ok=%v", err, tt.ok)
			}
			if err == nil {
				_ = kit.Close()
			}
		})
	}

	if _, err := NewKit(ctxT(t), nil); err == nil {
		t.Fatal("a kit with no log was built")
	}
}

// TestKitRecordsItsBrakeWaiver: a kit without brakes says so on its own stream.
func TestKitRecordsItsBrakeWaiver(t *testing.T) {
	b, _ := memBackends(t)
	kit, err := NewKit(ctxT(t), b.Log, WithWorkDir(t.TempDir()), Waive(GuardBrakes, "the host rate-limits"))
	if err != nil {
		t.Fatalf("kit: %v", err)
	}
	defer func() { _ = kit.Close() }()
	if ev := waivedEvents(t, b.Log, kit.SessionID()); len(ev) != 1 {
		t.Fatalf("recorded %d waivers, want 1", len(ev))
	}
	if kit.Grant().Allows("deploy") || !kit.Grant().Allows("bash") {
		t.Fatalf("kit grant = %v", kit.Grant().Actions())
	}
	if ctx := kit.Bind(context.Background(), capability.NewGrant("read")); ctx == nil {
		t.Fatal("bind returned no context")
	}
	if kit.Dispatcher() == nil || len(kit.Toolset()) == 0 {
		t.Fatal("kit offers no dispatcher or tools")
	}
}

// TestApprovalGateIsEffectiveByDefault: a policy given to a default run actually stops the
// action. The same script with no policy writes the file, so the refusal is the gate's.
func TestApprovalGateIsEffectiveByDefault(t *testing.T) {
	write := func() *llmtest.ScriptedModel {
		return llmtest.NewScripted(
			llmtest.CallTool("c1", "write", json.RawMessage(`{"path":"hello.txt","content":"hi"}`)),
			llmtest.SayText("done"),
		)
	}
	tests := []struct {
		name      string
		opts      []Option
		wantWrite bool
	}{
		{"no policy", nil, true},
		{"gated with nobody to ask", []Option{WithApproval([]string{"write"}, nil)}, false},
		{"gated and denied", []Option{WithApproval([]string{"write"}, newStubPrompter(mission.ApprovalDecision{}))}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			b, _ := memBackends(t)
			run, err := Assemble(ctxT(t), write(), b, append([]Option{WithWorkDir(dir)}, tt.opts...)...)
			if err != nil {
				t.Fatalf("assemble: %v", err)
			}
			defer func() { _ = run.Close() }()
			_, _ = run.Do(ctxT(t), goal.Spec{Objective: "write hello.txt"})
			_, statErr := os.Stat(filepath.Join(dir, "hello.txt"))
			if wrote := statErr == nil; wrote != tt.wantWrite {
				t.Fatalf("file written = %v, want %v", wrote, tt.wantWrite)
			}
		})
	}
}

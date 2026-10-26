package main

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/governed"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/llm/llmtest"
	"github.com/ionalpha/flynn/memory/guard"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/skill/skilltool"
	"github.com/ionalpha/flynn/state"
	"github.com/ionalpha/flynn/tools"
)

// TestUntrustedOutputToolsCoverEveryUntrustedTool is the gate the declaration of
// [untrustedOutputTools] names: a tool in a run's default toolset that declares less
// than full trust is work this binary does not vouch for, and its output has to taint
// the run's memories. A tool added without being listed fails here rather than
// writing clean-looking notes from whatever it read.
func TestUntrustedOutputToolsCoverEveryUntrustedTool(t *testing.T) {
	set := append(tools.New(nil).Tools(), skilltool.New(state.NewMemory().Skills()).Tools()...)
	for _, tool := range set {
		tw, ok := tool.(mission.TrustedWork)
		if !ok || tw.WorkTrust() == sandbox.TrustTrusted {
			continue
		}
		if name := tool.Def().Name; !untrustedOutputTools[name] {
			t.Errorf("tool %q declares %v trust and is not in untrustedOutputTools", name, tw.WorkTrust())
		}
	}
	if !untrustedOutputTools["bash"] {
		t.Error("the shell must taint: its network is open")
	}
}

func toolUse(name string) llm.Message {
	return llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{
		{Kind: llm.KindToolUse, ToolUse: &llm.ToolUse{ID: "1", Name: name, Input: json.RawMessage(`{}`)}},
	}}
}

// TestLearnedMemoryIsTaintedByTheShell writes through the memory stack the binary
// runs, under the context the distiller is handed, and holds the stack to stamping
// the run's taint on the item. It is the guarantee [taintFromTranscript] and the
// guard in [newMemoryStack] make together; either one missing writes the item clean.
func TestLearnedMemoryIsTaintedByTheShell(t *testing.T) {
	cases := []struct {
		name       string
		transcript []llm.Message
		carried    []string
		tainted    bool
	}{
		{"no tools", []llm.Message{llm.Text(llm.RoleAssistant, "done")}, nil, false},
		{"file tools only", []llm.Message{toolUse("read"), toolUse("edit")}, nil, false},
		{"shell", []llm.Message{toolUse("read"), toolUse("bash")}, nil, true},
		{"spawn", []llm.Message{toolUse(mission.ActionSpawn)}, nil, true},
		{"compacted shell", []llm.Message{toolUse("read")}, []string{"tool:bash"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mem := governed.NewMemoryStack(state.NewMemory().Memory(), nil).Store()
			ctx := taintFromTranscript(context.Background(), c.transcript, c.carried...)
			it, err := mem.Write(ctx, state.MemoryItem{Kind: "lesson", Content: "the build needs cgo", Sources: []string{guard.SchemeAgent + "run-1"}})
			if err != nil {
				t.Fatalf("write: %v", err)
			}
			if it.Tainted != c.tainted {
				t.Fatalf("Tainted = %v, want %v", it.Tainted, c.tainted)
			}
			want := guard.PushOnPromotion
			if c.tainted {
				want = guard.PushDenied
			}
			if got := guard.PushEligibility(it); got != want {
				t.Fatalf("eligibility = %v, want %v", got, want)
			}
		})
	}
}

// TestRememberIsNotTaintedByTheGuard: the guard sits under every write the stack makes,
// /remember included, and a pinned fact written outside any run's taint scope must
// stay the operator's, pushed at wake.
func TestRememberIsNotTaintedByTheGuard(t *testing.T) {
	mem := governed.NewMemoryStack(state.NewMemory().Memory(), nil).Store()
	var out strings.Builder
	if !rememberFact(context.Background(), &out, mem, "use tabs") {
		t.Fatalf("remember failed: %s", out.String())
	}
	items, err := mem.Recall(context.Background(), state.RecallQuery{})
	if err != nil || len(items) != 1 {
		t.Fatalf("recall = %v, %v", items, err)
	}
	if items[0].Tainted || guard.PushEligibility(items[0]) != guard.PushAllowed {
		t.Fatalf("pinned fact = %+v, want untainted and pushed", items[0])
	}
}

// TestCompactCarriesTheShellsTaint: /compact replaces the transcript with a summary of
// it, so the shell call that fetched what the summary repeats is gone from what the
// session distils. The taint has to survive the summary, and /clear, which carries
// nothing forward, drops it.
func TestCompactCarriesTheShellsTaint(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s, buf := newSlashSession(t, llmtest.NewScripted(
		llmtest.SayText("done"),
		llmtest.SayText("the user asked for the thing; it is done"),
	))
	if _, err := s.runTurn(ctx, "do the thing", nil, nil); err != nil {
		t.Fatalf("turn: %v\n%s", err, buf.String())
	}
	s.transcript = append(s.transcript, toolUse("bash"))
	if handled, err := s.replCommand(ctx, "/compact"); !handled || err != nil {
		t.Fatalf("/compact = (%v, %v)\n%s", handled, err, buf.String())
	}
	if !slices.Contains(s.carriedTaint, "tool:bash") {
		t.Fatalf("carriedTaint = %v after compacting a shell call", s.carriedTaint)
	}
	s.clear()
	if len(s.carriedTaint) != 0 {
		t.Fatalf("/clear kept %v", s.carriedTaint)
	}
}

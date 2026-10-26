package agent

import (
	"strings"
	"testing"

	"github.com/ionalpha/flynn/archetypes"
	"github.com/ionalpha/flynn/governed"
	"github.com/ionalpha/flynn/llm/llmtest"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/tools"
)

// noTools is an extension bridge that contributes nothing, enough to prove the host's
// bridge reaches the assembly.
type noTools struct{}

func (noTools) Tools() []mission.Tool { return nil }

// TestGoalNamesAModelItCannotResolve: Goal and GoalResult resolve Config.Model before
// assembling, so an unknown provider is an error and not a run on some default model.
func TestGoalNamesAModelItCannotResolve(t *testing.T) {
	a := New(Config{Model: "no-such-provider:x", WorkDir: t.TempDir()})
	if _, err := a.Goal(ctxT(t), "say so"); err == nil {
		t.Fatal("Goal ran on a model that does not resolve")
	}
	res, err := a.GoalResult(ctxT(t), "say so")
	if err == nil {
		t.Fatal("GoalResult ran on a model that does not resolve")
	}
	if res.RunID != "" {
		t.Fatalf("a run that never assembled reported id %q", res.RunID)
	}
}

// TestRunGoalRefusesABlankObjective: whitespace is no objective, and nothing is assembled.
func TestRunGoalRefusesABlankObjective(t *testing.T) {
	a := New(Config{WorkDir: t.TempDir()})
	_, err := a.runGoalResult(ctxT(t), llmtest.NewScripted(llmtest.SayText("unreachable")), "  \n")
	if err == nil || !strings.Contains(err.Error(), "empty objective") {
		t.Fatalf("err = %v, want an empty-objective error", err)
	}
}

// TestConfigExtensionsReachTheRun: a host's tool bridge is handed to the assembly, so a run
// with one assembles and converges.
func TestConfigExtensionsReachTheRun(t *testing.T) {
	a := New(Config{WorkDir: t.TempDir(), Extensions: noTools{}})
	res, err := a.runGoalResult(ctxT(t), llmtest.NewScripted(llmtest.SayText("ok")), "go")
	if err != nil || res.Stop != governed.StopConverged {
		t.Fatalf("result = %+v, err = %v", res, err)
	}
}

// TestToolGrantFollowsTheArchetype: a host toolset is granted by name, and an Agent
// archetype narrows it to the capabilities that archetype declares; the model call stays.
func TestToolGrantFollowsTheArchetype(t *testing.T) {
	sb, err := sandbox.NewLocal(t.TempDir(), sandbox.WithDefaultConfinement())
	if err != nil {
		t.Fatalf("sandbox: %v", err)
	}
	t.Cleanup(func() { _ = sb.Close() })
	all := tools.New(sb).Tools()
	if len(all) < 2 {
		t.Fatalf("need two tools to narrow, have %d", len(all))
	}
	kept, dropped := all[0].Def().Name, all[1].Def().Name

	open := New(Config{Policy: &Policy{Tools: all}}).toolGrant()
	if !open.Allows(kept) || !open.Allows(dropped) || !open.Allows(mission.ActionModelGenerate) {
		t.Fatal("without an archetype every host tool and the model call are granted")
	}

	narrow := New(Config{
		Agent:  &archetypes.Spec{Capabilities: []string{kept}},
		Policy: &Policy{Tools: all},
	}).toolGrant()
	if !narrow.Allows(kept) || !narrow.Allows(mission.ActionModelGenerate) {
		t.Fatal("the archetype's capability or the model call was dropped")
	}
	if narrow.Allows(dropped) {
		t.Fatalf("%s is outside the archetype's capabilities but was granted", dropped)
	}
}

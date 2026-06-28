package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/llm/llmtest"
	"github.com/ionalpha/flynn/mission"
)

// fanoutAgentModel is a stateless content-routing fake: the parent run fans out two
// children on its first turn and reports completion once their results fold back; a
// child run answers directly. Stateless so parent and children can call it
// concurrently through the real agent assembly.
type fanoutAgentModel struct{ parentMark string }

func (m fanoutAgentModel) Generate(_ context.Context, req llm.Request) (llm.Response, error) {
	first := firstUser(req.Messages)
	if strings.Contains(first, m.parentMark) {
		if hasResult(req.Messages) {
			return llmtest.SayText("delegated work complete"), nil
		}
		mk := func(id, obj string) llm.Block {
			in, _ := json.Marshal(mission.SubGoal{Objective: obj, Actions: []string{mission.ActionModelGenerate}})
			return llm.Block{Kind: llm.KindToolUse, ToolUse: &llm.ToolUse{ID: id, Name: mission.ActionSpawn, Input: in}}
		}
		return llm.Response{
			Message:    llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{mk("s1", "subtask one"), mk("s2", "subtask two")}},
			StopReason: llm.StopToolUse,
		}, nil
	}
	return llmtest.SayText("done: " + first), nil
}

func firstUser(msgs []llm.Message) string {
	for _, msg := range msgs {
		if msg.Role == llm.RoleUser {
			if t := msg.TextContent(); t != "" {
				return t
			}
		}
	}
	return ""
}

func hasResult(msgs []llm.Message) bool {
	for _, msg := range msgs {
		for _, b := range msg.Blocks {
			if b.Kind == llm.KindToolResult {
				return true
			}
		}
	}
	return false
}

// TestAgentFansOutEndToEnd is the product-level proof: the real agent assembly (the
// same path Goal takes) fans out two governed child runs via the wired spawner, runs
// them on the runtime, folds their results back, and converges. No fakes below the
// model; this is the agent actually delegating.
func TestAgentFansOutEndToEnd(t *testing.T) {
	a := New(Config{WorkDir: t.TempDir()})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	result, err := a.runGoal(ctx, fanoutAgentModel{parentMark: "delegate"}, "delegate two subtasks")
	if err != nil {
		t.Fatalf("runGoal: %v", err)
	}
	if !strings.Contains(result, "complete") {
		t.Fatalf("result = %q, want the parent's completion after folding children", result)
	}
}

package llmtest_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/llm/llmtest"
)

// llmtest is a test double, not a conformance suite: nothing runs against a
// host's implementation, so there is no suite that could fail to catch a
// defect. These tests pin the guarantees the double documents instead, since
// every agent test that scripts a model leans on them.

func req(text string) llm.Request {
	return llm.Request{Messages: []llm.Message{llm.Text(llm.RoleUser, text)}}
}

func TestScriptedReturnsTurnsInOrder(t *testing.T) {
	m := llmtest.NewScripted(llmtest.SayText("one"), llmtest.SayText("two"))
	for _, want := range []string{"one", "two"} {
		got, err := m.Generate(context.Background(), req("ignored"))
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		if text := got.Message.Blocks[0].Text; text != want {
			t.Fatalf("turn = %q, want %q", text, want)
		}
	}
	if m.Calls() != 2 {
		t.Fatalf("Calls = %d, want 2", m.Calls())
	}
}

func TestScriptedExhaustionIsTerminalAndStillRecorded(t *testing.T) {
	m := llmtest.NewScripted(llmtest.SayText("only"))
	if _, err := m.Generate(context.Background(), req("a")); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	_, err := m.Generate(context.Background(), req("b"))
	if !errors.Is(err, llmtest.ErrScriptExhausted) {
		t.Fatalf("past the script = %v, want ErrScriptExhausted", err)
	}
	if got := fault.Classify(err); got != fault.Terminal {
		t.Fatalf("class = %v, want Terminal", got)
	}
	if m.Calls() != 1 {
		t.Fatalf("Calls = %d, want 1: an exhausted call is not a served turn", m.Calls())
	}
	// The loop that overran the script is exactly what a test wants to inspect.
	if n := len(m.Requests()); n != 2 {
		t.Fatalf("recorded %d requests, want 2 including the overrun", n)
	}
}

func TestScriptedRecordsRequestsAsACopy(t *testing.T) {
	m := llmtest.NewScripted(llmtest.SayText("x"))
	if _, err := m.Generate(context.Background(), req("first")); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	got := m.Requests()
	if len(got) != 1 || got[0].Messages[0].Blocks[0].Text != "first" {
		t.Fatalf("Requests = %+v, want the one request", got)
	}
	got[0] = llm.Request{}
	if again := m.Requests(); len(again[0].Messages) != 1 {
		t.Fatal("mutating the returned slice changed the recorded requests")
	}
}

func TestTurnConstructors(t *testing.T) {
	say := llmtest.SayText("hi")
	if say.StopReason != llm.StopEndTurn || say.Message.Role != llm.RoleAssistant {
		t.Fatalf("SayText = %+v, want an assistant end-of-turn", say)
	}
	call := llmtest.CallTool("id1", "read", json.RawMessage(`{"p":1}`))
	if call.StopReason != llm.StopToolUse {
		t.Fatalf("CallTool stop = %v, want tool use", call.StopReason)
	}
	b := call.Message.Blocks
	if len(b) != 1 || b[0].Kind != llm.KindToolUse || b[0].ToolUse.ID != "id1" ||
		b[0].ToolUse.Name != "read" || string(b[0].ToolUse.Input) != `{"p":1}` {
		t.Fatalf("CallTool blocks = %+v", b)
	}
}

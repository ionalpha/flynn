package llmtest_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/llm/llmtest"
)

func call(id string) llm.Block {
	return llm.Block{Kind: llm.KindToolUse, ToolUse: &llm.ToolUse{ID: id, Name: "bash", Input: []byte(`{}`)}}
}

func result(id string) llm.Block {
	return llm.Block{Kind: llm.KindToolResult, ToolResult: &llm.ToolResult{ToolUseID: id, Content: "ok"}}
}

func msg(role llm.Role, blocks ...llm.Block) llm.Message {
	return llm.Message{Role: role, Blocks: blocks}
}

func TestCheckRequest(t *testing.T) {
	user := llm.Text(llm.RoleUser, "hi")
	cases := []struct {
		name string
		msgs []llm.Message
		want string // substring of the error; empty means accepted
	}{
		{"plain text", []llm.Message{user, llm.Text(llm.RoleAssistant, "hello")}, ""},
		{"answered call", []llm.Message{user, msg(llm.RoleAssistant, call("a")), msg(llm.RoleUser, result("a"))}, ""},
		{"parallel calls answered", []llm.Message{user, msg(llm.RoleAssistant, call("a"), call("b")), msg(llm.RoleUser, result("b"), result("a"))}, ""},
		{"call with nothing after", []llm.Message{user, msg(llm.RoleAssistant, call("a"))}, "no result"},
		{"call followed by text", []llm.Message{user, msg(llm.RoleAssistant, call("a")), llm.Text(llm.RoleAssistant, "done")}, "no result"},
		{"one of two answered", []llm.Message{user, msg(llm.RoleAssistant, call("a"), call("b")), msg(llm.RoleUser, result("a"))}, `"b"`},
		{"result for an unknown call", []llm.Message{user, msg(llm.RoleAssistant, call("a")), msg(llm.RoleUser, result("a"), result("z"))}, `"z"`},
		{"result with no call before", []llm.Message{user, msg(llm.RoleUser, result("a"))}, "did not make"},
		{"result from the assistant", []llm.Message{user, msg(llm.RoleAssistant, call("a")), msg(llm.RoleAssistant, result("a"))}, "not user"},
		{"call from the user", []llm.Message{msg(llm.RoleUser, call("a")), msg(llm.RoleUser, result("a"))}, "not assistant"},
		{"empty message", []llm.Message{user, {Role: llm.RoleAssistant}}, "no content"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := llmtest.CheckRequest(llm.Request{Messages: tc.msgs})
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("rejected a valid request: %v", err)
			case tc.want != "" && err == nil:
				t.Fatal("accepted a request a provider rejects")
			case tc.want != "" && !strings.Contains(err.Error(), tc.want):
				t.Fatalf("error %q does not name %q", err, tc.want)
			}
		})
	}
}

// TestScriptedRejectsAnInvalidRequest proves the scripted model enforces the check
// rather than only offering it, and does not spend a scripted turn on the refusal.
func TestScriptedRejectsAnInvalidRequest(t *testing.T) {
	m := llmtest.NewScripted(llmtest.SayText("one"))
	bad := llm.Request{Messages: []llm.Message{llm.Text(llm.RoleUser, "hi"), msg(llm.RoleAssistant, call("a"))}}
	if _, err := m.Generate(context.Background(), bad); err == nil || errors.Is(err, llmtest.ErrScriptExhausted) {
		t.Fatalf("Generate on an invalid request = %v, want a rejection", err)
	}
	resp, err := m.Generate(context.Background(), llm.Request{Messages: []llm.Message{llm.Text(llm.RoleUser, "hi")}})
	if err != nil || resp.Message.TextContent() != "one" {
		t.Fatalf("the refused call consumed the script: %v %+v", err, resp)
	}
}

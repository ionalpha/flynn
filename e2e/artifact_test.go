package e2e

import (
	"testing"
)

// TestGoalProducesArtifact drives a two-turn tool-using run: the model calls the write
// tool, the binary executes it inside the sandboxed workspace, feeds the tool result
// back, and the model finishes. It asserts the file physically appears with the right
// contents (the artifact a user asked for) and that the resulting record still verifies,
// so a real tool round-trip is proven end to end, not just a text reply.
func TestGoalProducesArtifact(t *testing.T) {
	fake := newFakeOpenAIQueue(
		t,
		toolCall("call_1", "write", `{"path":"answer.txt","content":"42\n"}`),
		finalText("Wrote answer.txt."),
	)
	in := newInstance(t).withModel(fake)

	res := in.run("-no-learn", "goal", "write 42 to answer.txt")
	requireExit(t, res, 0, "goal")

	got, err := in.workfile("answer.txt")
	if err != nil {
		t.Fatalf("expected artifact answer.txt in workspace: %v", err)
	}
	if string(got) != "42\n" {
		t.Fatalf("artifact contents: expected %q, got %q", "42\n", string(got))
	}

	// The tool result must have been fed back: the model made a second call, and the
	// second request carries a tool-role message.
	if fake.count() < 2 {
		t.Fatalf("expected at least 2 model calls (tool + fold), got %d", fake.count())
	}
	second := fake.request(t, 1)
	var sawToolResult bool
	for _, m := range second.Messages {
		if m.Role == "tool" {
			sawToolResult = true
		}
	}
	if !sawToolResult {
		t.Fatalf("second request carried no tool result message; roles were %v", roles(second))
	}

	runID := in.runID(res)
	requireExit(t, in.verify(runID), 0, "spine verify after tool run")
}

func roles(r oaiRequest) []string {
	var out []string
	for _, m := range r.Messages {
		out = append(out, m.Role)
	}
	return out
}

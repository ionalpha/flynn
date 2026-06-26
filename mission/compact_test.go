package mission

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/llm"
)

// bigText returns a text body large enough to push a transcript over a small budget.
func bigText(seed string) string { return seed + strings.Repeat("y", 4000) }

// userText / asstText / asstCall / userResult build the alternating turns a mission
// produces, so a test transcript matches the real shape (user, assistant, user, ...).
func userText(s string) llm.Message { return llm.Text(llm.RoleUser, s) }
func asstText(s string) llm.Message { return llm.Text(llm.RoleAssistant, s) }

func asstCall(id, name string) llm.Message {
	return llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{
		{Kind: llm.KindToolUse, ToolUse: &llm.ToolUse{ID: id, Name: name, Input: json.RawMessage(`{}`)}},
	}}
}

func userResult(id, content string) llm.Message {
	return llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{
		{Kind: llm.KindToolResult, ToolResult: &llm.ToolResult{ToolUseID: id, Content: content}},
	}}
}

func TestCompactDisabledOrShortIsUnchanged(t *testing.T) {
	msgs := []llm.Message{userText("go"), asstCall("1", "bash"), userResult("1", bigText("a")), asstText("done")}
	// budget 0 disables compaction.
	if got := compactView(msgs, 0); !reflect.DeepEqual(got, msgs) {
		t.Fatal("budget 0 must disable compaction")
	}
	// Too few messages to have a middle, even over budget.
	if got := compactView(msgs, 1); !reflect.DeepEqual(got, msgs) {
		t.Fatal("a short transcript must be returned unchanged")
	}
}

func TestCompactElidesMiddleKeepsHeadAndTail(t *testing.T) {
	msgs := []llm.Message{
		userText("the objective"),
		asstCall("1", "bash"), userResult("1", bigText("a")),
		asstCall("2", "bash"), userResult("2", bigText("b")),
		asstCall("3", "bash"), userResult("3", bigText("c")),
		asstText("nearly there"), userText("continue"),
	}
	out := compactView(msgs, 2000) // far below the transcript size: forces compaction

	if len(out) >= len(msgs) {
		t.Fatalf("expected compaction to shorten the transcript, %d -> %d", len(msgs), len(out))
	}
	// Head is the objective, carrying the elision note.
	if out[0].Role != llm.RoleUser || !strings.Contains(out[0].TextContent(), "elided") {
		t.Fatalf("head should be the objective with an elision note: %q", out[0].TextContent())
	}
	if !strings.Contains(out[0].TextContent(), "the objective") {
		t.Fatal("head must keep the original objective text")
	}
	// The kept tail begins at an assistant turn (so roles alternate and no tool result
	// is orphaned) and runs to the end unchanged.
	if out[1].Role != llm.RoleAssistant {
		t.Fatalf("tail must start at an assistant turn, got %s", out[1].Role)
	}
	if !reflect.DeepEqual(out[len(out)-1], msgs[len(msgs)-1]) {
		t.Fatal("the last turn must be preserved verbatim")
	}
	// Input untouched.
	if msgs[0].TextContent() != "the objective" {
		t.Fatal("compactView mutated its input head")
	}
}

// TestCompactProperties is the safety net: over random alternating transcripts and
// budgets, compaction never mutates the input, never grows the transcript, keeps
// roles strictly alternating from the user objective, and never orphans a tool
// result from the call that produced it.
func TestCompactProperties(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		msgs := []llm.Message{userText(rapid.StringMatching(`[a-z ]{1,12}`).Draw(rt, "obj"))}
		nEx := rapid.IntRange(0, 8).Draw(rt, "exchanges")
		for i := range nEx {
			big := rapid.Bool().Draw(rt, "big")
			body := "ok"
			if big {
				body = bigText(fmt.Sprintf("s%d", i))
			}
			if rapid.Bool().Draw(rt, "isTool") {
				id := fmt.Sprintf("t%d", i)
				msgs = append(msgs, asstCall(id, "bash"), userResult(id, body))
			} else {
				msgs = append(msgs, asstText("thinking"), userText(body))
			}
		}
		budget := rapid.IntRange(0, 20000).Draw(rt, "budget")

		before := deepCopyMessages(msgs)
		out := compactView(msgs, budget)

		if !reflect.DeepEqual(msgs, before) {
			rt.Fatal("compactView mutated its input")
		}
		if estimateTokens(out) > estimateTokens(msgs) {
			rt.Fatalf("compaction grew the transcript: %d -> %d", estimateTokens(msgs), estimateTokens(out))
		}
		// Roles strictly alternate, starting with the user objective.
		for i, m := range out {
			want := llm.RoleUser
			if i%2 == 1 {
				want = llm.RoleAssistant
			}
			if m.Role != want {
				rt.Fatalf("role at %d is %s, want %s (alternation broken)", i, m.Role, want)
			}
		}
		// No tool result is orphaned: every result's call appears earlier in the view.
		seen := map[string]bool{}
		for _, m := range out {
			for _, b := range m.Blocks {
				switch {
				case b.Kind == llm.KindToolUse && b.ToolUse != nil:
					seen[b.ToolUse.ID] = true
				case b.Kind == llm.KindToolResult && b.ToolResult != nil:
					if !seen[b.ToolResult.ToolUseID] {
						rt.Fatalf("tool result %s has no preceding call in the view", b.ToolResult.ToolUseID)
					}
				}
			}
		}
	})
}

// TestCompactTinyTranscriptDoesNotGrow is the regression for a rapid-found case: when
// the transcript is small enough that the elision note costs more than the turns it
// would replace, compaction must keep the original rather than produce a larger view.
func TestCompactTinyTranscriptDoesNotGrow(t *testing.T) {
	msgs := []llm.Message{userText("hi")}
	for range 4 {
		msgs = append(msgs, asstText("thinking"), userText("ok"))
	}
	out := compactView(msgs, 20) // a tiny budget the tiny transcript still exceeds
	if estimateTokens(out) > estimateTokens(msgs) {
		t.Fatalf("compaction grew a tiny transcript: %d -> %d", estimateTokens(msgs), estimateTokens(out))
	}
}

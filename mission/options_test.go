package mission

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/brakes"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/llm/llmtest"
)

// summarizingTool is a Tool that also describes its own large results, so a test can
// assert the executor finds the summarizer through the tool registry.
type summarizingTool struct{ name string }

func (s summarizingTool) Def() llm.Tool {
	return llm.Tool{Name: s.name, Description: "d", InputSchema: json.RawMessage(`{"type":"object"}`)}
}

func (summarizingTool) Invoke(context.Context, json.RawMessage) (string, error) { return "ok", nil }

func (summarizingTool) SummarizeResult(json.RawMessage, string) string { return "one line" }

// plainTool is a Tool with no summarizer capability.
type plainTool struct{ name string }

func (p plainTool) Def() llm.Tool {
	return llm.Tool{Name: p.name, Description: "d", InputSchema: json.RawMessage(`{"type":"object"}`)}
}

func (plainTool) Invoke(context.Context, json.RawMessage) (string, error) { return "ok", nil }

// TestSummarizerFor proves the executor resolves a tool's optional summarizer: present
// for a tool that offers one, nil for a tool that does not, and nil for a tool that was
// never registered.
func TestSummarizerFor(t *testing.T) {
	e := NewExecutor(llmtest.NewScripted(), WithTools(summarizingTool{name: "read"}, plainTool{name: "write"}))

	s := e.summarizerFor("read")
	if s == nil {
		t.Fatal("a tool that summarizes its results must be found")
	}
	if got := s.SummarizeResult(json.RawMessage(`{}`), "body"); got != "one line" {
		t.Fatalf("summary = %q", got)
	}
	if e.summarizerFor("write") != nil {
		t.Fatal("a tool with no summarizer must resolve to nil")
	}
	if e.summarizerFor("unregistered") != nil {
		t.Fatal("an unknown tool must resolve to nil")
	}
}

// TestWithMaxTokens proves the per-turn output cap is taken and that a non-positive
// value is ignored, so a caller cannot accidentally cap a turn at zero tokens.
func TestWithMaxTokens(t *testing.T) {
	def := NewExecutor(llmtest.NewScripted()).maxTokens
	if got := NewExecutor(llmtest.NewScripted(), WithMaxTokens(4096)).maxTokens; got != 4096 {
		t.Fatalf("maxTokens = %d, want 4096", got)
	}
	for _, bad := range []int{0, -1} {
		if got := NewExecutor(llmtest.NewScripted(), WithMaxTokens(bad)).maxTokens; got != def {
			t.Fatalf("WithMaxTokens(%d) must keep the default %d, got %d", bad, def, got)
		}
	}
}

// TestWithBrakes proves a brake hook is wired into the waist and a nil hook is ignored,
// so the standalone agent stays zero-config rather than installing a nil hook.
func TestWithBrakes(t *testing.T) {
	if e := NewExecutor(llmtest.NewScripted(), WithBrakes(nil)); e.brakes {
		t.Fatal("a nil brake hook must be ignored")
	}
	h := brakes.NewHook(brakes.Limits{}, brakes.NewMemSwitch())
	e := NewExecutor(llmtest.NewScripted(), WithBrakes(h))
	if !e.brakes {
		t.Fatal("a brake hook must be recorded as installed")
	}
	if len(e.dispatchOpts) == 0 {
		t.Fatal("a brake hook must be wired into the dispatch waist")
	}
}

// TestToolArg proves the elision digest names an action by its salient argument, with
// path winning over pattern and command, a long command truncated, and nothing reported
// for a call whose input carries none of them.
func TestToolArg(t *testing.T) {
	long := strings.Repeat("a", 60)
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"path", `{"path":"go.mod"}`, "go.mod"},
		{"pattern", `{"pattern":"TODO"}`, "TODO"},
		{"command", `{"command":"go test ./..."}`, "go test ./..."},
		{"path wins over pattern", `{"path":"go.mod","pattern":"TODO"}`, "go.mod"},
		{"long command truncated", `{"command":"` + long + `"}`, long[:40]},
		{"no salient argument", `{"limit":3}`, ""},
		{"empty input", ``, ""},
		{"malformed input", `not-json`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := toolArg(json.RawMessage(c.input)); got != c.want {
				t.Fatalf("toolArg(%s) = %q, want %q", c.input, got, c.want)
			}
		})
	}
}

// TestElisionDigestNamesTheWork proves the digest lists the distinct tool calls the
// elided turns ran, deduplicated, and says nothing when they ran no tools.
func TestElisionDigestNamesTheWork(t *testing.T) {
	if got := elisionDigest(nil); got != "" {
		t.Fatalf("no elided turns must yield no digest, got %q", got)
	}
	text := []llm.Message{llm.Text(llm.RoleAssistant, "just thinking")}
	if got := elisionDigest(text); got != "" {
		t.Fatalf("elided turns with no tool calls must yield no digest, got %q", got)
	}

	msgs := []llm.Message{
		callMsg("1", "read"),
		useMsg("2", "read", `{"path":"go.mod"}`),
		useMsg("3", "read", `{"path":"go.mod"}`), // duplicate, elided once
	}
	got := elisionDigest(msgs)
	if !strings.Contains(got, "read go.mod") || strings.Count(got, "read go.mod") != 1 {
		t.Fatalf("digest must name the distinct work once: %q", got)
	}
}

// TestElisionDigestCapsActions proves a very long gap is summarized to a bounded list
// with a count of the rest, so the digest cannot itself grow the transcript.
func TestElisionDigestCapsActions(t *testing.T) {
	var msgs []llm.Message
	for i := range 14 {
		msgs = append(msgs, useMsg(string(rune('a'+i)), "read", `{"path":"f`+string(rune('a'+i))+`"}`))
	}
	got := elisionDigest(msgs)
	if !strings.Contains(got, "and 4 more") {
		t.Fatalf("digest must report the actions beyond the cap: %q", got)
	}
	if strings.Count(got, "read f") != 10 {
		t.Fatalf("digest must list exactly the capped number of actions: %q", got)
	}
}

// useMsg builds an assistant tool_use message with the given JSON input.
func useMsg(id, name, input string) llm.Message {
	return llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{
		{Kind: llm.KindToolUse, ToolUse: &llm.ToolUse{ID: id, Name: name, Input: json.RawMessage(input)}},
	}}
}

// TestBlockSize proves the token estimate measures each block kind's payload and treats
// a block whose payload pointer is absent as empty rather than panicking.
func TestBlockSize(t *testing.T) {
	cases := []struct {
		name  string
		block llm.Block
		want  int
	}{
		{"text", llm.Block{Kind: llm.KindText, Text: "hello"}, 5},
		{"tool use", llm.Block{Kind: llm.KindToolUse, ToolUse: &llm.ToolUse{Name: "read", Input: json.RawMessage(`{}`)}}, 6},
		{"tool use with no payload", llm.Block{Kind: llm.KindToolUse}, 0},
		{"tool result", llm.Block{Kind: llm.KindToolResult, ToolResult: &llm.ToolResult{Content: "abc"}}, 3},
		{"tool result with no payload", llm.Block{Kind: llm.KindToolResult}, 0},
		{"opaque", llm.Block{Kind: llm.KindOpaque, Raw: json.RawMessage(`{"a":1}`)}, 7},
		{"image", llm.Block{Kind: llm.KindImage}, imageTokens * charsPerToken},
		{"unknown kind", llm.Block{Kind: "who-knows"}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := blockSize(c.block); got != c.want {
				t.Fatalf("blockSize = %d, want %d", got, c.want)
			}
		})
	}
}

// TestNopObserversDrop proves the zero-config defaults accept every event without a
// consumer, so a standalone run never depends on an observer being wired.
func TestNopObserversDrop(_ *testing.T) {
	ctx := context.Background()
	nopGenerationRecorder{}.RecordGeneration(ctx, GenerationEnvelope{Pinned: true, Seed: 7})
	nopReporter{}.Report(ctx, Event{Kind: EventTurnCompleted, Goal: "g"})
}

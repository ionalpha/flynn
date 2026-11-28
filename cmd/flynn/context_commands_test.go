package main

import (
	"context"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/llm/llmtest"
)

// TestClearResetsRun proves /clear forgets the current run and any carried summary, so
// the next turn opens a fresh conversation.
func TestClearResetsRun(t *testing.T) {
	s := &replSession{
		started: true, runID: "abc", lastSeq: 5,
		transcript:     []llm.Message{llm.Text(llm.RoleUser, "hi")},
		carriedContext: "old summary",
	}
	s.clear()
	if s.started || s.runID != "" || s.lastSeq != 0 || len(s.transcript) != 0 || s.carriedContext != "" {
		t.Fatalf("clear did not reset the run: %+v", s)
	}
}

// TestCompactSummarizesAndCarries proves /compact summarizes the transcript, resets the
// run, and carries the summary forward so the next run continues with less context.
func TestCompactSummarizesAndCarries(t *testing.T) {
	s := &replSession{
		started: true, model: constModel{text: "a tidy summary"},
		transcript: []llm.Message{llm.Text(llm.RoleUser, "do a thing"), llm.Text(llm.RoleAssistant, "did it")},
	}
	n, err := s.compact(context.Background())
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if n != 2 {
		t.Fatalf("compact reported %d messages, want 2", n)
	}
	if s.started || s.runID != "" {
		t.Fatal("compact did not reset the run")
	}
	if !strings.Contains(s.carriedContext, "a tidy summary") {
		t.Fatalf("carried context missing the summary: %q", s.carriedContext)
	}
}

// TestCompactNothingToDo proves compacting before any turn reports there is nothing to
// compact rather than making an empty model call.
func TestCompactNothingToDo(t *testing.T) {
	s := &replSession{model: constModel{text: "x"}}
	if _, err := s.compact(context.Background()); err == nil {
		t.Fatal("compact with no started run should error")
	}
}

// TestShellClearAndCompact drives both commands through the full-screen session: they run
// as commands (not model turns), reset the run, and report their outcome.
func TestShellClearAndCompact(t *testing.T) {
	host, ui := newHostForTest(t, constModel{text: "the summary"})

	host.submit("do something", nil)
	waitIdle(t, host)
	if !host.s.started {
		t.Fatal("first turn did not start a run")
	}

	host.submit("/compact", nil)
	waitIdle(t, host)
	if host.s.started {
		t.Fatal("/compact did not reset the run")
	}
	if !strings.Contains(ui.transcript(), "compacted") {
		t.Fatalf("/compact was not reported:\n%s", ui.transcript())
	}
	if !strings.Contains(host.s.carriedContext, "the summary") {
		t.Fatalf("/compact did not carry the summary forward: %q", host.s.carriedContext)
	}

	host.submit("another", nil)
	waitIdle(t, host)
	host.submit("/clear", nil)
	waitIdle(t, host)
	if host.s.started || host.s.carriedContext != "" {
		t.Fatal("/clear did not reset the run and drop the summary")
	}
	if !strings.Contains(ui.transcript(), "context cleared") {
		t.Fatalf("/clear was not reported:\n%s", ui.transcript())
	}
}

// requestModel records the last request it was sent.
type requestModel struct {
	text string
	got  *llm.Request
}

func (m requestModel) Generate(_ context.Context, req llm.Request) (llm.Response, error) {
	*m.got = req
	if err := llmtest.CheckRequest(req); err != nil {
		return llm.Response{}, err
	}
	return llm.Response{Message: llm.Text(llm.RoleAssistant, m.text), StopReason: llm.StopEndTurn}, nil
}

// TestCompactSendsNoUnansweredToolUse holds the request a provider will accept: the
// session transcript records tool calls without their results, and a tool_use block with
// no tool_result after it is an HTTP 400 from Anthropic. The calls must still reach the
// summarizer as text, or the summary cannot say what was done.
func TestCompactSendsNoUnansweredToolUse(t *testing.T) {
	var req llm.Request
	s := &replSession{
		started: true, model: requestModel{text: "summary", got: &req},
		transcript: []llm.Message{
			llm.Text(llm.RoleUser, "look around"),
			{Role: llm.RoleAssistant, Blocks: []llm.Block{
				{Kind: llm.KindToolUse, ToolUse: &llm.ToolUse{ID: "t1", Name: "bash", Input: []byte(`{"cmd":"ls"}`)}},
				{Kind: llm.KindToolUse, ToolUse: &llm.ToolUse{ID: "t2", Name: "read", Input: []byte(`{"path":"` + strings.Repeat("x", 2*toolInputLimit) + `"}`)}},
			}},
			llm.Text(llm.RoleAssistant, "found it"),
		},
	}
	if _, err := s.compact(context.Background()); err != nil {
		t.Fatalf("compact: %v", err)
	}
	var sent strings.Builder
	for _, m := range req.Messages {
		if len(m.ToolUses()) > 0 {
			t.Fatalf("compact sent a raw tool_use block: %+v", m)
		}
		sent.WriteString(m.TextContent())
	}
	for _, want := range []string{"look around", "[called bash", `"cmd":"ls"`, "[called read", "found it"} {
		if !strings.Contains(sent.String(), want) {
			t.Errorf("summarizer request is missing %q:\n%s", want, sent.String())
		}
	}
	if strings.Contains(sent.String(), strings.Repeat("x", 2*toolInputLimit)) {
		t.Error("a tool call's arguments were not cut to toolInputLimit")
	}
}

// TestCompactKeepsTheEarlierSummary: a second /compact summarizes the carried summary
// along with the new turns, or everything before the first compaction is lost.
func TestCompactKeepsTheEarlierSummary(t *testing.T) {
	var req llm.Request
	s := &replSession{
		started: true, model: requestModel{text: "second summary", got: &req},
		carriedContext: "the first summary",
		transcript:     []llm.Message{llm.Text(llm.RoleUser, "next ask")},
	}
	if _, err := s.compact(context.Background()); err != nil {
		t.Fatalf("compact: %v", err)
	}
	if len(req.Messages) != 1 || !strings.Contains(req.Messages[0].TextContent(), "the first summary") {
		t.Fatalf("second compaction dropped the earlier summary: %+v", req.Messages)
	}
	if !strings.Contains(s.carriedContext, "second summary") {
		t.Fatalf("carried context = %q", s.carriedContext)
	}
}

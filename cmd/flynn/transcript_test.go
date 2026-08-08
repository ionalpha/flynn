package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/internal/tui/theme"
	"github.com/ionalpha/flynn/session"
)

// TestTranscriptViewToolCallDetail proves a tool call names the target it acts on,
// and that a write or edit renders the diff of the change, not just the tool name.
func TestTranscriptViewToolCallDetail(t *testing.T) {
	v := newTranscriptView(theme.Default())

	if got := renderLines(v, session.Event{Kind: session.KindToolCall, Tool: "read", Input: json.RawMessage(`{"path":"go.mod"}`)}); !strings.Contains(got, "read") || !strings.Contains(got, "go.mod") {
		t.Fatalf("read call did not name its target: %q", got)
	}
	if got := renderLines(v, session.Event{Kind: session.KindToolCall, Tool: "grep", Input: json.RawMessage(`{"pattern":"TODO"}`)}); !strings.Contains(got, "TODO") {
		t.Fatalf("grep call did not name its pattern: %q", got)
	}
	got := renderLines(v, session.Event{Kind: session.KindToolCall, Tool: "write", Input: json.RawMessage(`{"path":"NEW.md","content":"hello\nworld\n"}`)})
	if !strings.Contains(got, "NEW.md") || !strings.Contains(got, "hello") || !strings.Contains(got, "world") {
		t.Fatalf("write call did not render its content as a diff: %q", got)
	}
	// "one" and "two" share no prefix or suffix, so the word-level highlight leaves
	// each whole rather than splitting it, keeping the assertion robust.
	got = renderLines(v, session.Event{Kind: session.KindToolCall, Tool: "edit", Input: json.RawMessage(`{"path":"F.md","old":"one","new":"two"}`)})
	if !strings.Contains(got, "one") || !strings.Contains(got, "two") {
		t.Fatalf("edit call did not render a diff of the change: %q", got)
	}
}

// renderLines renders one event through a fresh transcript view and joins the
// result, so a test can assert on the text a single event produces.
func renderLines(v *transcriptView, ev session.Event) string {
	return strings.Join(v.lines(ev, 80), "\n")
}

// TestTranscriptShortNumericAnswerNotDropped proves a short answer markdown would parse
// as an empty ordered-list item (a bare "16.") is still shown, rather than swallowed to
// a blank the way goldmark drops it.
func TestTranscriptShortNumericAnswerNotDropped(t *testing.T) {
	v := newTranscriptView(theme.Default())
	for _, ans := range []string{"16.", "3.", "8."} {
		got := renderLines(v, session.Event{Kind: session.KindAssistant, Text: ans})
		if !strings.Contains(got, strings.TrimSuffix(ans, ".")) {
			t.Fatalf("short answer %q rendered empty: %q", ans, got)
		}
	}
}

// TestTranscriptViewConversation covers the conversation events: the model's
// prose renders (as markdown), a tool call names the tool, and a clean tool
// result and the per-turn bookkeeping stay out of the transcript.
func TestTranscriptViewConversation(t *testing.T) {
	v := newTranscriptView(theme.Default())

	if got := renderLines(v, session.Event{Kind: session.KindAssistant, Text: "hello **world**"}); !strings.Contains(got, "hello") || !strings.Contains(got, "world") {
		t.Fatalf("assistant prose not rendered: %q", got)
	}
	if got := renderLines(v, session.Event{Kind: session.KindToolCall, Tool: "read_file"}); !strings.Contains(got, "read_file") {
		t.Fatalf("tool call not rendered: %q", got)
	}
	if got := renderLines(v, session.Event{Kind: session.KindToolResult, Tool: "read_file", Result: "ok"}); got != "" {
		t.Fatalf("a clean tool result must stay out of the transcript, got %q", got)
	}
	if got := renderLines(v, session.Event{Kind: session.KindTurnCompleted, Turn: 1}); got != "" {
		t.Fatalf("turn.completed must fold into the badge, not the transcript, got %q", got)
	}
	if got := renderLines(v, session.Event{Kind: session.KindActionAdmitted, Action: "read_file", Trust: "semi-trusted"}); got != "" {
		t.Fatalf("an admitted action folds into the badge, not the transcript, got %q", got)
	}
}

// TestTranscriptViewGovernanceAndFailures proves the boundary events surface
// inline: a failed tool result, a refused action with its fault class, and an
// action that ran but errored.
func TestTranscriptViewGovernanceAndFailures(t *testing.T) {
	v := newTranscriptView(theme.Default())

	if got := renderLines(v, session.Event{Kind: session.KindToolResult, Tool: "write_file", Result: "denied", IsError: true}); !strings.Contains(got, "write_file") || !strings.Contains(got, "failed") {
		t.Fatalf("failed tool result not surfaced: %q", got)
	}
	rej := renderLines(v, session.Event{Kind: session.KindActionRejected, Action: "net.dial", Fault: "capability_denied"})
	if !strings.Contains(rej, "net.dial") || !strings.Contains(rej, "rejected") || !strings.Contains(rej, "capability_denied") {
		t.Fatalf("rejection not surfaced with its fault class: %q", rej)
	}
	if got := renderLines(v, session.Event{Kind: session.KindActionCompleted, Action: "fetch", Fault: "transient"}); !strings.Contains(got, "fetch") || !strings.Contains(got, "transient") {
		t.Fatalf("errored completion not surfaced: %q", got)
	}
	// A rejection with no recorded fault still reads cleanly.
	if got := renderLines(v, session.Event{Kind: session.KindActionRejected, Action: "spawn"}); !strings.Contains(got, "denied") {
		t.Fatalf("faultless rejection should fall back to a neutral reason: %q", got)
	}
}

// TestTranscriptViewConvergedDedup proves the terminal answer is not echoed
// twice when it repeats the last assistant message, but a distinct summary is
// shown, and a stall renders its reason.
func TestTranscriptViewConvergedDedup(t *testing.T) {
	v := newTranscriptView(theme.Default())
	_ = renderLines(v, session.Event{Kind: session.KindAssistant, Text: "the final answer"})
	if got := renderLines(v, session.Event{Kind: session.KindConverged, Text: "the final answer"}); got != "" {
		t.Fatalf("converged text repeating the last message must not echo, got %q", got)
	}

	v = newTranscriptView(theme.Default())
	_ = renderLines(v, session.Event{Kind: session.KindAssistant, Text: "working on it"})
	if got := renderLines(v, session.Event{Kind: session.KindConverged, Text: "distinct summary"}); !strings.Contains(got, "distinct summary") {
		t.Fatalf("a converged summary that differs must render, got %q", got)
	}

	if got := renderLines(v, session.Event{Kind: session.KindStalled, Err: "out of budget"}); !strings.Contains(got, "stalled") || !strings.Contains(got, "out of budget") {
		t.Fatalf("stall not rendered: %q", got)
	}
}

// TestStatusBadge proves the badge reflects the projection: the record state,
// the containment posture, the turn count, the blocked count, and the in-flight
// count, with the action hint trailing.
func TestStatusBadge(t *testing.T) {
	th := theme.Default()

	// A fresh run shows its record state and the idle hint.
	idle := statusBadge(th, session.NewProjection(), false, 0)
	if !strings.Contains(idle, "recording") || !strings.Contains(idle, "enter sends") {
		t.Fatalf("idle badge missing record state or hint: %q", idle)
	}

	p := session.NewProjection()
	for _, ev := range []session.Event{
		{Kind: session.KindActionAdmitted, Trust: "semi-trusted"},
		{Kind: session.KindActionAdmitted, Trust: "semi-trusted"},
		{Kind: session.KindActionCompleted},
		{Kind: session.KindActionRejected, Trust: "semi-trusted", Fault: "needs_approval"},
		{Kind: session.KindTurnCompleted, Usage: &session.Usage{InputTokens: 1200, OutputTokens: 300}},
		{Kind: session.KindRecordSealed},
	} {
		p = session.Reduce(p, ev)
	}
	busy := statusBadge(th, p, true, 2)
	for _, want := range []string{"sealed", "semi-trusted code", "1 blocked", "1 turns", "1.2k in", "working...", "2 queued"} {
		if !strings.Contains(busy, want) {
			t.Fatalf("busy badge missing %q:\n%s", want, busy)
		}
	}
}

// TestActivityFor covers both halves of the in-flight line: which events map to one,
// and that the live component draws the current line and nothing when idle.
func TestActivityFor(t *testing.T) {
	if line, ok := activityFor(session.Event{Kind: session.KindToolCall, Tool: "grep"}); !ok || !strings.Contains(line, "grep") {
		t.Fatalf("tool call activity = %q, %v", line, ok)
	}
	if line, ok := activityFor(session.Event{Kind: session.KindToolResult}); !ok || !strings.Contains(line, "thinking") {
		t.Fatalf("tool result activity = %q, %v", line, ok)
	}
	if _, ok := activityFor(session.Event{Kind: session.KindAssistant, Text: "x"}); ok {
		t.Fatal("assistant text must not change the activity line")
	}

	a := &activity{th: theme.Default()}
	if got := a.Render(80); got != nil {
		t.Fatalf("an idle activity renders nothing, got %v", got)
	}
	a.set("running grep...")
	if got := a.Render(80); len(got) != 1 || !strings.Contains(got[0], "running grep") {
		t.Fatalf("activity render = %v", got)
	}
}

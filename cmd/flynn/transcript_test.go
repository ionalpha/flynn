package main

import (
	"strings"
	"testing"

	"github.com/ionalpha/flynn/internal/tui/theme"
	"github.com/ionalpha/flynn/session"
)

// renderLines renders one event through a fresh transcript view and joins the
// result, so a test can assert on the text a single event produces.
func renderLines(v *transcriptView, ev session.Event) string {
	return strings.Join(v.lines(ev, 80), "\n")
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
	if got := renderLines(v, session.Event{Kind: session.KindActionAdmitted, Action: "read_file", Trust: "workspace"}); got != "" {
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
		{Kind: session.KindActionAdmitted, Trust: "workspace"},
		{Kind: session.KindActionAdmitted, Trust: "workspace"},
		{Kind: session.KindActionCompleted},
		{Kind: session.KindActionRejected, Trust: "workspace", Fault: "needs_approval"},
		{Kind: session.KindTurnCompleted, Usage: &session.Usage{InputTokens: 1200, OutputTokens: 300}},
		{Kind: session.KindRecordSealed},
	} {
		p = session.Reduce(p, ev)
	}
	busy := statusBadge(th, p, true, 2)
	for _, want := range []string{"sealed", "trust workspace", "1 blocked", "1 turns", "1.2k in", "working...", "2 queued"} {
		if !strings.Contains(busy, want) {
			t.Fatalf("busy badge missing %q:\n%s", want, busy)
		}
	}
}

// TestActivityFor maps events to the in-flight line, and TestActivityRender
// proves the live component draws the current line and nothing when idle.
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

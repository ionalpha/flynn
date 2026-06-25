package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/session"
)

func TestRenderEvent(t *testing.T) {
	cases := []struct {
		name    string
		ev      session.Event
		verbose bool
		want    []string
		absent  []string
	}{
		{"objective", session.Event{Kind: session.KindSessionStarted, Text: "do the thing"}, false, []string{"goal: do the thing"}, nil},
		{"turn", session.Event{Kind: session.KindTurnStarted, Turn: 2}, false, []string{"turn 2"}, nil},
		{"tool call default hides args", session.Event{Kind: session.KindToolCall, Tool: "bash", Input: json.RawMessage(`{"command":"ls"}`)}, false, []string{"-> bash"}, []string{"command", "ls"}},
		{"tool call verbose shows args", session.Event{Kind: session.KindToolCall, Tool: "bash", Input: json.RawMessage(`{"command":"ls"}`)}, true, []string{"-> bash", "command", "ls"}, nil},
		{"tool result hidden by default", session.Event{Kind: session.KindToolResult, Tool: "bash", Result: "the output"}, false, nil, []string{"the output"}},
		{"tool result shown verbose", session.Event{Kind: session.KindToolResult, Tool: "bash", Result: "the output"}, true, []string{"the output"}, nil},
		// The guarantee that matters: a tool failure is shown even at the default
		// verbosity, so an error is never silent.
		{"tool error always shown", session.Event{Kind: session.KindToolResult, Tool: "bash", Result: "permission denied", IsError: true}, false, []string{"bash failed", "permission denied"}, nil},
		{"stop reason verbose only", session.Event{Kind: session.KindTurnCompleted, Turn: 1, StopReason: "tool_use"}, true, []string{"tool_use"}, nil},
		{"stop reason hidden default", session.Event{Kind: session.KindTurnCompleted, Turn: 1, StopReason: "tool_use"}, false, nil, []string{"tool_use"}},
		{"converged final answer", session.Event{Kind: session.KindConverged, Text: "all done"}, false, []string{"all done"}, nil},
		{"stalled shows reason", session.Event{Kind: session.KindStalled, Err: "quota exhausted"}, false, []string{"stalled", "quota exhausted"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var b strings.Builder
			renderEvent(&b, tc.ev, tc.verbose)
			out := b.String()
			for _, w := range tc.want {
				if !strings.Contains(out, w) {
					t.Fatalf("output %q missing %q", out, w)
				}
			}
			for _, a := range tc.absent {
				if strings.Contains(out, a) {
					t.Fatalf("output %q should not contain %q", out, a)
				}
			}
		})
	}
}

func TestOneLineCollapsesAndTruncates(t *testing.T) {
	if got := oneLine("a\n  b\tc", 100); got != "a b c" {
		t.Fatalf("collapse: %q", got)
	}
	if got := oneLine("abcdef", 3); got != "abc..." {
		t.Fatalf("truncate: %q", got)
	}
}

package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"pgregory.net/rapid"

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

func TestRenderTurnCompletedShowsUsage(t *testing.T) {
	// Usage is shown at any verbosity (it is the point of the line), with the cache
	// hit-rate derived from the cached share of input.
	ev := session.Event{Kind: session.KindTurnCompleted, Turn: 1, Usage: &session.Usage{
		InputTokens: 2000, OutputTokens: 500, CacheReadTokens: 1500, CacheWriteTokens: 400,
	}}
	var b strings.Builder
	renderEvent(&b, ev, false)
	out := b.String()
	for _, w := range []string{"2.0k in", "500 out", "75% cached", "cache write"} {
		if !strings.Contains(out, w) {
			t.Fatalf("usage line %q missing %q", out, w)
		}
	}
}

func TestFormatUsage(t *testing.T) {
	cases := []struct {
		name string
		u    session.Usage
		want []string
		no   []string
	}{
		{"plain", session.Usage{InputTokens: 120, OutputTokens: 30}, []string{"120 in", "30 out"}, []string{"cached", "cache write"}},
		{"cached", session.Usage{InputTokens: 1000, OutputTokens: 50, CacheReadTokens: 900}, []string{"1.0k in", "90% cached"}, []string{"cache write"}},
		{"no cache pct when zero read", session.Usage{InputTokens: 1000, OutputTokens: 50}, []string{"1.0k in"}, []string{"cached"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := formatUsage(tc.u)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Fatalf("%q missing %q", got, w)
				}
			}
			for _, n := range tc.no {
				if strings.Contains(got, n) {
					t.Fatalf("%q should not contain %q", got, n)
				}
			}
		})
	}
}

func TestUsageMeterSummary(t *testing.T) {
	var m usageMeter
	if m.summary() != "" {
		t.Fatal("empty meter should summarize to nothing")
	}
	m.add(session.Usage{InputTokens: 1000, OutputTokens: 200, CacheReadTokens: 500})
	m.add(session.Usage{InputTokens: 1000, OutputTokens: 100, CacheReadTokens: 1000})
	got := m.summary()
	// Totals: 2000 in, 300 out, 1500 cached -> 75%.
	for _, w := range []string{"tokens:", "2.0k in", "300 out", "75% cached"} {
		if !strings.Contains(got, w) {
			t.Fatalf("summary %q missing %q", got, w)
		}
	}
}

func TestHumanTokens(t *testing.T) {
	for in, want := range map[int]string{0: "0", 999: "999", 1000: "1.0k", 1234: "1.2k", 45600: "45.6k"} {
		if got := humanTokens(in); got != want {
			t.Fatalf("humanTokens(%d) = %q, want %q", in, got, want)
		}
	}
}

// TestUsageFormatProperty checks the usage math is panic-free and well-bounded for
// any non-negative token counts, including the edges the live readout will hit: a
// turn with zero input, or cached reads recorded against zero input. A cache
// percentage, when shown, is always within 0..100, and a meter's totals equal the
// sum of the turns folded into it.
func TestUsageFormatProperty(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		mk := func(label string) session.Usage {
			return session.Usage{
				InputTokens:      rapid.IntRange(0, 5_000_000).Draw(rt, label+"-in"),
				OutputTokens:     rapid.IntRange(0, 1_000_000).Draw(rt, label+"-out"),
				CacheReadTokens:  rapid.IntRange(0, 5_000_000).Draw(rt, label+"-cr"),
				CacheWriteTokens: rapid.IntRange(0, 5_000_000).Draw(rt, label+"-cw"),
			}
		}

		// formatUsage must never panic and, when it reports a cache share, keep it in
		// range. The cached count can exceed input in adversarial draws; the format
		// must still not produce a percentage above 100 or below 0.
		u := mk("u")
		out := formatUsage(u)
		if u.InputTokens > 0 && u.CacheReadTokens > 0 {
			pct := u.CacheReadTokens * 100 / u.InputTokens
			if pct <= 100 && !strings.Contains(out, fmt.Sprintf("%d%% cached", pct)) {
				rt.Fatalf("expected %d%% cached in %q", pct, out)
			}
		}

		// A meter's totals are the exact sum of the turns folded in.
		var m usageMeter
		n := rapid.IntRange(0, 6).Draw(rt, "turns")
		var in, outT, cr, cw int
		for i := range n {
			tu := mk(fmt.Sprintf("t%d", i))
			m.add(tu)
			in, outT, cr, cw = in+tu.InputTokens, outT+tu.OutputTokens, cr+tu.CacheReadTokens, cw+tu.CacheWriteTokens
		}
		if m.input != in || m.output != outT || m.cacheRead != cr || m.cacheWrite != cw {
			rt.Fatalf("meter totals drifted: got %+v, want in=%d out=%d cr=%d cw=%d", m, in, outT, cr, cw)
		}
		// summary is empty exactly when there is nothing worth showing.
		if (in == 0 && outT == 0) != (m.summary() == "") {
			rt.Fatalf("summary emptiness wrong for totals in=%d out=%d: %q", in, outT, m.summary())
		}
	})
}

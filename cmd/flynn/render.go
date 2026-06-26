package main

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/ionalpha/flynn/session"
)

// renderEvent writes one session event to w as a line of the run transcript. It is
// the single renderer shared by a live `flynn goal`, by `flynn inspect` replaying a
// recorded run, and (later) by the interactive session, so a run reads the same
// however it is viewed. verbose adds the detail the default view omits for brevity:
// the model's own narration, each tool's arguments and output, and why each turn
// stopped. A tool error is shown at any verbosity, since a failure must never be
// silent.
func renderEvent(w io.Writer, ev session.Event, verbose bool) {
	switch ev.Kind {
	case session.KindSessionStarted:
		_, _ = fmt.Fprintf(w, "goal: %s\n", ev.Text)
	case session.KindTurnStarted:
		_, _ = fmt.Fprintf(w, "  turn %d\n", ev.Turn)
	case session.KindAssistant:
		if verbose {
			if t := strings.TrimSpace(ev.Text); t != "" {
				_, _ = fmt.Fprintf(w, "    %s\n", oneLine(t, 200))
			}
		}
	case session.KindToolCall:
		if verbose {
			_, _ = fmt.Fprintf(w, "  -> %s %s\n", ev.Tool, oneLine(string(ev.Input), 120))
		} else {
			_, _ = fmt.Fprintf(w, "  -> %s\n", ev.Tool)
		}
	case session.KindToolResult:
		switch {
		case ev.IsError:
			_, _ = fmt.Fprintf(w, "  !! %s failed: %s\n", ev.Tool, oneLine(ev.Result, 200))
		case verbose:
			_, _ = fmt.Fprintf(w, "     %s\n", oneLine(ev.Result, 200))
		}
	case session.KindTurnCompleted:
		if verbose && ev.StopReason != "" {
			_, _ = fmt.Fprintf(w, "  (turn %d ended: %s)\n", ev.Turn, ev.StopReason)
		}
		if ev.Usage != nil {
			_, _ = fmt.Fprintf(w, "  %s\n", formatUsage(*ev.Usage))
		}
	case session.KindConverged:
		_, _ = fmt.Fprintf(w, "\n%s\n", ev.Text)
	case session.KindStalled:
		_, _ = fmt.Fprintf(w, "\nstalled: %s\n", ev.Err)
	}
}

// usageMeter accumulates per-turn token usage across a run so the renderer can
// print a running session total. It is the live counterpart of the per-turn lines:
// the same numbers the spine records, summed for the whole run.
type usageMeter struct {
	turns                                int
	input, output, cacheRead, cacheWrite int
}

// add folds one turn's usage into the running total.
func (m *usageMeter) add(u session.Usage) {
	m.turns++
	m.input += u.InputTokens
	m.output += u.OutputTokens
	m.cacheRead += u.CacheReadTokens
	m.cacheWrite += u.CacheWriteTokens
}

// summary renders the run total as a single line, or "" if no turn reported usage
// (a backend that does not surface token counts), so the line is shown only when it
// carries real numbers.
func (m *usageMeter) summary() string {
	if m.turns == 0 || (m.input == 0 && m.output == 0) {
		return ""
	}
	return "tokens: " + formatUsage(session.Usage{
		InputTokens:      m.input,
		OutputTokens:     m.output,
		CacheReadTokens:  m.cacheRead,
		CacheWriteTokens: m.cacheWrite,
	})
}

// formatUsage renders one usage record compactly: input and output token counts,
// and the cache-hit-rate (the cached share of input, the prompt-cache win) plus the
// tokens written to cache when any. A backend that reports no counts renders as
// zeros, so the line is only emitted by callers that have real usage to show.
func formatUsage(u session.Usage) string {
	s := fmt.Sprintf("%s in / %s out", humanTokens(u.InputTokens), humanTokens(u.OutputTokens))
	if u.InputTokens > 0 && u.CacheReadTokens > 0 {
		pct := u.CacheReadTokens * 100 / u.InputTokens
		s += fmt.Sprintf(" (%d%% cached)", pct)
	}
	if u.CacheWriteTokens > 0 {
		s += fmt.Sprintf(" (+%s cache write)", humanTokens(u.CacheWriteTokens))
	}
	return s
}

// humanTokens renders a token count compactly: exact below 1000, otherwise to one
// decimal in thousands (1234 -> "1.2k"), so a long run's totals stay readable.
func humanTokens(n int) string {
	if n < 1000 {
		return strconv.Itoa(n)
	}
	return fmt.Sprintf("%.1fk", float64(n)/1000)
}

// oneLine collapses whitespace runs (including newlines) into single spaces and
// truncates to max runes, so a multi-line tool output or model message renders as a
// single tidy transcript line.
func oneLine(s string, limit int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit]) + "..."
}

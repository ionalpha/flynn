package main

import (
	"fmt"
	"io"
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
	case session.KindConverged:
		_, _ = fmt.Fprintf(w, "\n%s\n", ev.Text)
	case session.KindStalled:
		_, _ = fmt.Fprintf(w, "\nstalled: %s\n", ev.Err)
	}
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

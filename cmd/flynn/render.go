package main

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/ionalpha/flynn/internal/projectctx"
	"github.com/ionalpha/flynn/internal/text"

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
	block := func(k blockKind, s string) {
		for _, l := range layBlock(nil, k, s, 0) {
			_, _ = fmt.Fprintln(w, l)
		}
	}
	switch ev.Kind {
	case session.KindSessionStarted:
		block(blockUser, ev.Text)
	case session.KindTurnStarted:
		block(blockNotice, fmt.Sprintf("turn %d", ev.Turn))
	case session.KindAssistant:
		if verbose {
			if t := strings.TrimSpace(ev.Text); t != "" {
				_, _ = fmt.Fprintln(w, "  "+text.Inert(oneLine(t, 200)))
			}
		}
	case session.KindToolCall:
		detail := toolTarget(ev.Input)
		if verbose {
			detail = oneLine(string(ev.Input), 120)
		}
		_, _ = fmt.Fprintln(w, toolHead(nil, ev.Tool, detail, 0))
	case session.KindToolResult:
		switch {
		case ev.IsError:
			block(blockToolError, ev.Tool+" failed: "+oneLine(ev.Result, 200))
		case verbose:
			block(blockToolResult, ev.Tool+": "+oneLine(ev.Result, 200))
		}
	case session.KindTurnCompleted:
		if verbose && ev.StopReason != "" {
			block(blockNotice, fmt.Sprintf("turn %d ended: %s", ev.Turn, ev.StopReason))
		}
		if ev.Usage != nil {
			block(blockNotice, formatUsage(*ev.Usage))
		}
	case session.KindConverged:
		_, _ = fmt.Fprintln(w)
		_, _ = fmt.Fprintln(w, text.Inert(ev.Text))
	case session.KindStalled:
		_, _ = fmt.Fprintln(w)
		block(blockError, "stalled: "+ev.Err)
	case session.KindProjectContext:
		block(blockNotice, projectContextLine(ev))
	case session.KindSelection:
		block(blockNotice, selectionLine(ev))
	default:
		// Governance and record events (action.*, record.*) share the run's stream but
		// are not shown in the flat text transcript this renderer draws; the interactive
		// session renders them inline and in its panels.
	}
}

// selectionLine is the transcript line for a run's skill and tool selection. It is the
// readout of what the run was offered, drawn from the recorded event so a live run and
// a later `flynn inspect` say the same thing.
func selectionLine(ev session.Event) string {
	return "selection: " + ev.Reason + "; " + strings.ReplaceAll(ev.Text, "\n", "; ")
}

// projectContextLine is the transcript line for a project-file trust decision: whether
// the instruction files were loaded, on what basis, and which. The hashes stay in the
// record; a person reading the transcript needs the names.
func projectContextLine(ev session.Event) string {
	var names []string
	for _, l := range strings.Split(ev.Text, "\n") {
		if f := strings.Fields(l); len(f) > 0 {
			names = append(names, f[0])
		}
	}
	state := "not loaded"
	if ev.Allowed {
		state = "loaded as untrusted context"
	}
	line := fmt.Sprintf("project files %s (%s): %s", state, ev.Reason, strings.Join(names, ", "))
	if !ev.Allowed && ev.Reason == string(projectctx.BasisUnattended) {
		line += "; --project-files=trust loads them"
	}
	return line
}

// usageMeter is a run's token total and turn count, printed as a closing line. The
// sum is [session.Usage.Add], the same one a replayed [session.Projection] uses.
type usageMeter struct {
	turns int
	total session.Usage
}

// add folds one turn's usage into the running total.
func (m *usageMeter) add(u session.Usage) {
	m.turns++
	m.total = m.total.Add(u)
}

// summary renders the run total as a single line, or "" if no turn reported usage
// (a backend that does not surface token counts), so the line is shown only when it
// carries real numbers.
func (m *usageMeter) summary() string {
	if m.turns == 0 || (m.total.InputTokens == 0 && m.total.OutputTokens == 0) {
		return ""
	}
	return "tokens: " + formatUsage(m.total)
}

// inOut is the "X in / Y out" pair shared by the per-turn line and the badge.
func inOut(u session.Usage) string {
	return fmt.Sprintf("%s in / %s out", humanTokens(u.InputTokens), humanTokens(u.OutputTokens))
}

// formatUsage renders one usage record compactly: input and output token counts,
// and the cache-hit-rate (the cached share of input, the prompt-cache win) plus the
// tokens written to cache when any. A backend that reports no counts renders as
// zeros, so the line is only emitted by callers that have real usage to show.
func formatUsage(u session.Usage) string {
	s := inOut(u)
	if u.InputTokens > 0 && u.CacheReadTokens > 0 {
		s += fmt.Sprintf(" (%d%% cached)", u.Tokens().CacheHitPercent())
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

// oneLine collapses a multi-line tool output or model message into a single
// tidy transcript line.
func oneLine(s string, limit int) string { return text.OneLine(s, limit) }

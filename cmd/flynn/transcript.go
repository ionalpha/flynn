package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/ionalpha/flynn/internal/text"
	"github.com/ionalpha/flynn/internal/tui/render"
	"github.com/ionalpha/flynn/internal/tui/screen"
	"github.com/ionalpha/flynn/internal/tui/theme"
	"github.com/ionalpha/flynn/session"
)

// transcriptView renders the typed session event stream into themed scrollback
// lines for the interactive shell. It is the shell's counterpart to the flat
// text renderEvent writes for `flynn goal`: instead of one collapsed line per
// event, the model's prose renders as markdown and governance boundaries render
// inline with the action they belong to. It owns no run state beyond what one
// event needs to render (the last prose shown, to avoid echoing a converged
// answer twice), so the shell can throw it away between sessions.
type transcriptView struct {
	th *theme.Theme
	md *render.Markdown

	// lastAssistant is the most recent assistant message rendered, trimmed. The
	// terminal convergence event repeats the model's final message as the run's
	// result; comparing against it drops that one duplicate without hiding a
	// summary that genuinely differs from the last thing said.
	lastAssistant string
}

// newTranscriptView binds a renderer to a theme.
func newTranscriptView(th *theme.Theme) *transcriptView {
	return &transcriptView{th: th, md: render.NewMarkdown(th)}
}

// contentGutter is the left inset shared by the transcript's prose and diffs, so
// they line up with the tool and report lines (already inset by the same amount)
// instead of sitting flush against the terminal's left edge. On the right it runs
// to the window's last column.
const contentGutter = 2

// inset prefixes each content line with the gutter, so a block of prose or a diff
// shares the transcript's left margin. An empty input stays empty.
func inset(lines []string) []string {
	if len(lines) == 0 {
		return nil
	}
	prefix := strings.Repeat(" ", contentGutter)
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = prefix + l
	}
	return out
}

// bodyWidth is the width available to guttered content: the frame width less the
// gutter, floored at 1 so a narrow terminal still renders something.
func bodyWidth(width int) int {
	if w := width - contentGutter; w > 0 {
		return w
	}
	return 1
}

// prose renders an answer as markdown within the gutter, falling back to the plain
// text when markdown yields nothing for a non-empty source. A bare "16." parses as an
// empty ordered-list item and renders to no lines, so a short numeric answer would be
// dropped; the fallback shows it verbatim instead.
func (v *transcriptView) prose(src string, width int) []string {
	if lines := inset(v.md.Render(src, bodyWidth(width))); len(lines) > 0 {
		return lines
	}
	return inset(strings.Split(strings.TrimRight(text.Inert(src), "\n"), "\n"))
}

// lines is [transcriptView.block] rendered at width, or nil when the event
// carries nothing the transcript shows.
func (v *transcriptView) lines(ev session.Event, width int) []string {
	if b := v.block(ev); b != nil {
		return b.Render(width)
	}
	return nil
}

// block returns the transcript block for one event, or nil when it shows nothing.
// What is shown is decided now, in event order; the layout is left to the shell,
// which renders the block again after a width change.
//
// The conversation events (assistant prose, tool calls, failures) render
// directly; governance admissions and the per-turn bookkeeping fold into the
// status badge instead, so the transcript stays the conversation and the
// boundaries that interrupt it, not a log of every governed call.
func (v *transcriptView) block(ev session.Event) screen.Component {
	switch ev.Kind {
	case session.KindAssistant:
		text := strings.TrimSpace(ev.Text)
		if text == "" {
			return nil
		}
		v.lastAssistant = text
	case session.KindConverged:
		text := strings.TrimSpace(ev.Text)
		if text == "" || text == v.lastAssistant {
			return nil
		}
	case session.KindActionCompleted:
		// A clean completion is silent: the tool result already showed its outcome.
		if ev.Fault == "" {
			return nil
		}
	case session.KindToolCall, session.KindToolResult, session.KindActionRejected,
		session.KindStalled, session.KindProjectContext, session.KindSelection:
	default:
		// session.started (the shell echoes the prompt itself), turn.started,
		// turn.completed, and action.admitted carry no transcript line; their
		// content lives in the status badge.
		return nil
	}
	return screen.Func(func(width int) []string { return v.render(ev, width) })
}

// render lays out an event that [transcriptView.block] chose to show. It reads
// nothing that changes between events, so it may run on the paint goroutine.
func (v *transcriptView) render(ev session.Event, width int) []string {
	switch ev.Kind {
	case session.KindAssistant, session.KindConverged:
		return v.prose(strings.TrimSpace(ev.Text), width)
	case session.KindToolCall:
		return v.toolCall(ev, width)
	case session.KindToolResult:
		if ev.IsError {
			return layBlock(v.th, blockToolError, ev.Tool+" failed: "+oneLine(ev.Result, 200), width)
		}
		return []string{layRow(v.th, blockToolResult, ev.Tool+": "+resultSummary(ev.Result), width)}
	case session.KindActionRejected:
		// A refused action is the run hitting a governance boundary: always shown,
		// next to the work it stopped, with the denial's fault class.
		return layBlock(v.th, blockRejected, actionLabel(ev)+" rejected: "+faultText(ev.Fault), width)
	case session.KindActionCompleted:
		// One that ran but errored surfaces the fault class as a warning.
		return layBlock(v.th, blockWarning, actionLabel(ev)+" failed: "+ev.Fault, width)
	case session.KindStalled:
		return layBlock(v.th, blockError, "stalled: "+ev.Err, width)
	case session.KindProjectContext:
		// The trust decision for the repository's instruction files is shown wherever the
		// run is, so a session never reads files its user was not told about.
		return layBlock(v.th, blockNotice, projectContextLine(ev), width)
	case session.KindSelection:
		return layBlock(v.th, blockNotice, selectionLine(ev), width)
	default:
		return nil
	}
}

// toolCall renders a tool invocation: a header naming the tool and the target it
// acts on (the file it touches, the pattern it searches, the command it runs), and
// for a write or edit the diff of the change, so the transcript shows what the agent
// did rather than only that it called a tool.
func (v *transcriptView) toolCall(ev session.Event, width int) []string {
	header := toolHead(v.th, ev.Tool, toolTarget(ev.Input), width)
	diff := v.toolDiff(ev, width)
	if len(diff) == 0 {
		return []string{header}
	}
	// A diff is a block, not a one-liner. A blank line before the header and after the
	// diff set the whole change apart from the tool calls above and the reply below, so
	// it does not read as squashed between them.
	out := append([]string{"", header}, diff...)
	return append(out, "")
}

// toolTarget pulls the one argument worth naming from a tool call's input: the file
// it touches, the pattern it searches, or the command it runs. It returns "" when the
// tool takes none of these, so the header just names the tool.
func toolTarget(input json.RawMessage) string {
	var a struct {
		Path    string `json:"path"`
		Pattern string `json:"pattern"`
		Command string `json:"command"`
	}
	_ = json.Unmarshal(input, &a)
	switch {
	case a.Path != "":
		return a.Path
	case a.Pattern != "":
		return a.Pattern
	case a.Command != "":
		return oneLine(a.Command, 60)
	}
	return ""
}

// toolDiff renders the change a write or edit makes as a themed diff, so the
// transcript shows the exact lines added and removed. A write diffs against an empty
// file, so a new file reads as all additions. Other tools carry no diff.
func (v *transcriptView) toolDiff(ev session.Event, width int) []string {
	switch ev.Tool {
	case "write":
		var a struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		}
		if json.Unmarshal(ev.Input, &a) != nil || a.Path == "" {
			return nil
		}
		return inset(render.Diff(v.th, a.Path, "", a.Content, bodyWidth(width)))
	case "edit":
		var a struct {
			Path string `json:"path"`
			Old  string `json:"old"`
			New  string `json:"new"`
		}
		if json.Unmarshal(ev.Input, &a) != nil || a.Path == "" {
			return nil
		}
		return inset(render.Diff(v.th, a.Path, a.Old, a.New, bodyWidth(width)))
	}
	return nil
}

// actionLabel names the governed action for an inline governance line, falling
// back to a neutral word when the waist recorded no name.
func actionLabel(ev session.Event) string {
	if ev.Action != "" {
		return ev.Action
	}
	return "action"
}

// faultText renders a denial's fault class, or a neutral phrase when the waist
// recorded none, so a rejection line is never left dangling after the colon.
func faultText(fault string) string {
	if fault == "" {
		return "denied"
	}
	return fault
}

// statusBadge renders the always-on run status from its projection: the record
// state, the containment posture, the turn count and token spend, and any
// governance boundary the run hit. It is a pure function of the projection and
// the shell's live busy/queue state, styled by role so a theme restyles it
// without touching this. Segments with nothing to say are omitted, so a run that
// has not started yet shows only its record state.
//
// width is the row it must fit: hints are dropped lowest priority first until the
// line fits, so a narrow terminal keeps the badge and the most useful keys instead
// of cutting the row off mid-word. 0 keeps every hint.
func statusBadge(th *theme.Theme, p session.Projection, busy bool, queued, width int) string {
	var segs []string

	segs = append(segs, th.Render(recordRole(p.Record), string(p.Record)))
	if p.Containment != "" {
		// The containment posture: how far the last governed action was trusted, which
		// sets the sandbox it runs under. Rendered as "<level> code" so it reads as a
		// plain statement (trusted / semi-trusted / untrusted code) rather than the
		// doubled-up "trust trusted".
		segs = append(segs, th.Render(theme.Trust, p.Containment+" code"))
	}
	if inflight := p.Admitted - p.Completed - p.Rejected; inflight > 0 {
		segs = append(segs, th.Render(theme.StatusBusy, fmt.Sprintf("%d running", inflight)))
	}
	if p.Rejected > 0 {
		segs = append(segs, th.Render(theme.Rejected, fmt.Sprintf("%d blocked", p.Rejected)))
	}
	if p.Turns > 0 {
		segs = append(segs, th.Render(theme.Muted, fmt.Sprintf("%d turns", p.Turns)))
	}
	if tokens := badgeTokens(p.Usage); tokens != "" {
		segs = append(segs, th.Render(theme.Muted, tokens))
	}

	badge := strings.Join(segs, th.Render(theme.Muted, " · "))
	gap := ""
	if badge != "" {
		gap = "   "
	}
	room := 0
	if width > 0 {
		if room = width - screen.Width(badge+gap); room < 1 {
			return badge
		}
	}
	hint := fitHints(statusHints(busy, queued), room)
	if hint == "" {
		return badge
	}
	return badge + gap + hint
}

// recordRole maps a record state to its badge role, so the state's color tracks
// its meaning (recording, sealed, verified) through the theme.
func recordRole(state session.RecordState) theme.Role {
	switch state {
	case session.RecordSealed:
		return theme.RecordSealed
	case session.RecordVerified:
		return theme.RecordVerified
	default:
		return theme.RecordRecording
	}
}

// badgeTokens renders the run's cumulative token spend compactly for the badge,
// or "" when no turn reported usage (a backend that surfaces no counts), so the
// segment appears only when it carries real numbers.
func badgeTokens(u session.Usage) string {
	if u.InputTokens == 0 && u.OutputTokens == 0 {
		return ""
	}
	return inOut(u)
}

// activity is the shell's in-flight indicator: the one-line live region shown
// above the composer while a turn runs, naming what the agent is doing now (a
// tool it called, or thinking between tools). It is repainted in place each
// frame, so it never joins the scrollback; the finished work lands there as the
// transcript. Its own mutex guards the text, so the paint goroutine's Render and
// the turn goroutine's set never race, and Render never reaches back into the
// shell.
type activity struct {
	th *theme.Theme

	mu   sync.Mutex
	text string
}

// set replaces the activity line; an empty string renders nothing.
func (a *activity) set(text string) {
	a.mu.Lock()
	a.text = text
	a.mu.Unlock()
}

// Render draws the current activity line, styled as busy status, or nothing when
// idle.
func (a *activity) Render(int) []string {
	a.mu.Lock()
	text := a.text
	a.mu.Unlock()
	if text == "" {
		return nil
	}
	return []string{a.th.Render(theme.StatusBusy, "  "+text)}
}

// activityFor maps an event to the activity line it implies, or "" to leave the
// current line unchanged. A tool call names the tool running; a result or a new
// turn returns to the neutral thinking line.
func activityFor(ev session.Event) (string, bool) {
	switch ev.Kind {
	case session.KindToolCall:
		return "running " + text.Inert(ev.Tool) + "...", true
	case session.KindToolResult:
		return "thinking...", true
	case session.KindTurnStarted:
		return "thinking...", true
	default:
		return "", false
	}
}

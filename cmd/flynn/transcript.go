package main

import (
	"fmt"
	"strings"
	"sync"

	"github.com/ionalpha/flynn/internal/tui/render"
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

// lines renders one event into the scrollback lines that follow it, wrapped to
// width, or nil when the event carries nothing the transcript shows. The
// conversation events (assistant prose, tool calls, failures) render directly;
// governance admissions and the per-turn bookkeeping fold into the status badge
// instead, so the transcript stays the conversation and the boundaries that
// interrupt it, not a log of every governed call.
func (v *transcriptView) lines(ev session.Event, width int) []string {
	switch ev.Kind {
	case session.KindAssistant:
		text := strings.TrimSpace(ev.Text)
		if text == "" {
			return nil
		}
		v.lastAssistant = text
		return v.md.Render(text, width)
	case session.KindToolCall:
		return []string{v.th.Render(theme.ToolName, "  → "+ev.Tool)}
	case session.KindToolResult:
		if ev.IsError {
			return []string{v.th.Render(theme.Rejected, "  !! "+ev.Tool+" failed: "+oneLine(ev.Result, 200))}
		}
		return nil
	case session.KindActionRejected:
		// A refused action is the run hitting a governance boundary: always shown,
		// next to the work it stopped, with the denial's fault class.
		return []string{v.th.Render(theme.Rejected, "  ✗ "+actionLabel(ev)+" rejected: "+faultText(ev.Fault))}
	case session.KindActionCompleted:
		// A clean completion is silent (the tool result already showed its outcome);
		// one that ran but errored surfaces the fault class as a warning.
		if ev.Fault == "" {
			return nil
		}
		return []string{v.th.Render(theme.Warning, "  !! "+actionLabel(ev)+" failed: "+ev.Fault)}
	case session.KindConverged:
		text := strings.TrimSpace(ev.Text)
		if text == "" || text == v.lastAssistant {
			return nil
		}
		return v.md.Render(text, width)
	case session.KindStalled:
		return []string{v.th.Render(theme.Error, "  stalled: "+ev.Err)}
	default:
		// session.started (the shell echoes the prompt itself), turn.started,
		// turn.completed, and action.admitted carry no transcript line; their
		// content lives in the status badge.
		return nil
	}
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
func statusBadge(th *theme.Theme, p session.Projection, busy bool, queued int) string {
	var segs []string

	segs = append(segs, th.Render(recordRole(p.Record), string(p.Record)))
	if p.Containment != "" {
		segs = append(segs, th.Render(theme.Trust, "trust "+p.Containment))
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
	hint := statusHint(busy, queued)
	if badge == "" {
		return hint
	}
	return badge + th.Render(theme.Muted, "  —  ") + hint
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
	return fmt.Sprintf("%s in / %s out", humanTokens(u.InputTokens), humanTokens(u.OutputTokens))
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
		return "running " + ev.Tool + "...", true
	case session.KindToolResult:
		return "thinking...", true
	case session.KindTurnStarted:
		return "thinking...", true
	default:
		return "", false
	}
}

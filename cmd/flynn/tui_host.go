package main

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/internal/tui/editor"
	"github.com/ionalpha/flynn/internal/tui/input"
	"github.com/ionalpha/flynn/internal/tui/screen"
	tuiterm "github.com/ionalpha/flynn/internal/tui/term"
	"github.com/ionalpha/flynn/internal/tui/theme"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/session"
)

// shellUI is the slice of the shell the session host drives. An interface so
// tests can observe the host through a recording fake.
type shellUI interface {
	Append(lines ...string)
	SetLive(c screen.Component)
	SetStatus(line string)
	SetCapture(fn func(input.Event) bool)
	Quit()
	Suspend(run func())
	Draft() string
	SetDraft(text string)
	PasteImage(att editor.Attachment)
	Width() int
}

// shellRunner is the confined executor behind the composer's shell mode: the
// slice of the sandbox the host needs, an interface so tests can substitute
// a recording fake.
type shellRunner interface {
	Exec(ctx context.Context, cmd sandbox.Command) (sandbox.ExecResult, error)
}

// sessionHost owns the session policy on top of the shell's mechanics: it
// echoes and queues submitted prompts, drives each one as a turn of the
// replSession on its own goroutine, routes the turn's rendered output into
// the scrollback, and maps Escape and Ctrl+C onto cancelling the in-flight
// turn. Turns are strictly serial; a prompt submitted while one runs waits
// its turn in the queue. A prompt starting with "!" is a shell command: it
// holds the same turn slot, runs through the confined sandbox at the working
// directory, and lands its output in the scrollback like any other turn.
type sessionHost struct {
	ctx    context.Context
	s      *replSession
	ui     shellUI
	th     *theme.Theme
	run    shellRunner
	runErr error
	// edit runs the user's editor over the given text and returns the
	// result; the terminal mode round trip lives inside it. Nil (no
	// terminal to hand over) leaves Ctrl+G unclaimed.
	edit func(initial string) (string, error)
	// clip reads images off the OS clipboard for Ctrl+V and /paste. Nil (a
	// host with no clipboard) leaves both unclaimed.
	clip tuiterm.Clipboard

	// tv renders the typed event stream into themed transcript lines, and live is
	// the in-flight indicator repainted above the composer while a turn runs. Both
	// are the shell's rendering of the session events the turn driver taps through
	// the host's observer.
	tv   *transcriptView
	live *activity
	// panel is the governance overlay toggled with Ctrl+O, an on-demand expansion of
	// the status badge that reads the same run projection. It renders nothing while
	// hidden, so it can sit permanently in the live region alongside the activity
	// line without showing until the user opens it.
	panel *govPanel
	// approval is the interactive approval overlay, the top of the live stack. It
	// renders nothing until a governed action pauses for a human decision, at which
	// point the run goroutine arms it through promptApproval and installs it as the
	// shell's modal input capture, so the operator's keypress (allow/deny) resolves
	// the paused action.
	approval *approvalPrompt
	// timing is the clock the approval prompt reads for its countdown and re-arms
	// its repaint timer on, injectable so a test drives the grace-period display
	// deterministically. Nil means the system clock.
	timing clock.Timing
	// liveComp is the composite installed as the shell's live region for the whole
	// session: the governance panel above the activity line. Each part renders
	// nothing when it has nothing to show, so re-setting this component is how the
	// host repaints the live region after toggling the panel or clearing the
	// activity line.
	liveComp screen.Component

	mu       sync.Mutex
	busy     bool
	queue    []queuedTurn
	cancel   context.CancelFunc
	quitting bool
	turns    sync.WaitGroup
	// proj is the run status folded from the event stream, the source for the
	// status badge. It persists across turns (turn count, spend, record state
	// accumulate over the whole session) and is read and written under mu.
	proj session.Projection

	// rendered reports whether the current turn has appended any transcript line
	// yet, so a turn that produces no visible output (a weak model that emits only
	// whitespace) is reported rather than leaving a blank. It is reset when a turn
	// starts and set by onEvent, both on the single serial turn goroutine.
	rendered bool
}

// queuedTurn is one submitted prompt waiting behind a running turn: its text
// and the images attached to it, held together so a turn queued while another
// runs keeps its pictures.
type queuedTurn struct {
	text   string
	images []llm.Image
}

// liveStack composes several live-region components top to bottom into one, so the
// shell can hold a fixed live region whose parts each render themselves (or nothing)
// independently: the governance panel over the activity line.
type liveStack []screen.Component

// Render draws each part in order, skipping the ones with nothing to show this frame.
func (s liveStack) Render(width int) []string {
	var out []string
	for _, c := range s {
		out = append(out, c.Render(width)...)
	}
	return out
}

// pokeLive re-installs the live region to request a repaint after the host changed a
// part's state directly (toggling the panel, clearing the activity line), since those
// mutate a component's own state without going through the shell.
func (h *sessionHost) pokeLive() { h.ui.SetLive(h.liveComp) }

// greet writes the session banner and, for a resumed run, its rendered
// history, so the conversation's context is in the scrollback from the start.
func (h *sessionHost) greet() {
	h.liveComp = liveStack{h.approval, h.panel, h.live}
	h.ui.SetLive(h.liveComp)
	h.ui.Append(h.th.Render(theme.Status, "flynn interactive session in "+h.s.cwd))
	// A resumed run replays through the same renderer a live turn uses and folds into
	// the same projection, so it opens looking as it did live: the conversation and its
	// tool calls, with the turn and token totals in the badge, not a verbose event log.
	if h.s.started && h.s.runID != "" {
		h.seedHistory()
	}
	h.panel.set(h.proj)
	h.refreshStatus()
}

// seedHistory replays a resumed run's recorded events into the scrollback through the
// live transcript view and folds each into the projection. The opening prompt is echoed
// the way the shell echoes a live prompt (the transcript view leaves prompts to the
// shell); the rest render as the conversation and its tool calls, while the per-turn
// bookkeeping folds into the badge rather than printing verbose lines.
func (h *sessionHost) seedHistory() {
	events, err := session.History(h.ctx, h.s.store.Log(), h.s.runID)
	if err != nil {
		return
	}
	width := h.ui.Width()
	for _, ev := range events {
		h.proj = session.Reduce(h.proj, ev)
		if ev.Kind == session.KindSessionStarted {
			if t := strings.TrimSpace(ev.Text); t != "" {
				h.echoPrompt(t)
			}
			continue
		}
		if lines := h.tv.lines(ev, width); len(lines) > 0 {
			h.ui.Append(lines...)
		}
	}
}

// echoPrompt writes the user's input to the scrollback as the head of a new block:
// a leading blank line sets it apart from the block before, and a trailing blank
// separates the reply or command output that follows from the line that triggered
// it, so each exchange reads as its own group rather than one dense wall.
func (h *sessionHost) echoPrompt(text string) {
	h.ui.Append("", h.th.Render(theme.UserPrefix, "> ")+h.th.Render(theme.UserText, text), "")
}

// onEvent renders one session event as the turn produces it: it folds the event
// into the run projection that feeds the status badge, updates the in-flight
// activity line, and appends the event's transcript lines to the scrollback. It
// runs on the turn goroutine (turns are serial, so it is never re-entered), and
// calls into the shell only with no host lock held.
func (h *sessionHost) onEvent(ev session.Event) {
	h.mu.Lock()
	h.proj = session.Reduce(h.proj, ev)
	p := h.proj
	h.mu.Unlock()

	// The panel reads its own snapshot, so an open panel tracks the run as events
	// arrive; a closed panel renders nothing and pays only the snapshot copy.
	h.panel.set(p)
	if line, ok := activityFor(ev); ok {
		h.live.set(line)
	}
	if lines := h.tv.lines(ev, h.ui.Width()); len(lines) > 0 {
		h.ui.Append(lines...)
		h.rendered = true
	}
	h.refreshStatus()
}

// foldRecord folds a record lifecycle event into the run projection and repaints the
// badge and panel. Sealing and verifying run in-process at idle, off the subscribed
// event stream, so the shell folds their outcome into the projection exactly as onEvent
// folds a streamed event, keeping the badge the single source of the record state.
func (h *sessionHost) foldRecord(ev session.Event) {
	h.mu.Lock()
	h.proj = session.Reduce(h.proj, ev)
	p := h.proj
	h.mu.Unlock()
	h.panel.set(p)
	h.refreshStatus()
}

// refreshStatus rewrites the status badge from the run projection and the
// shell's live busy/queue state.
func (h *sessionHost) refreshStatus() {
	h.mu.Lock()
	p, busy, queued := h.proj, h.busy, len(h.queue)
	h.mu.Unlock()
	h.ui.SetStatus(statusBadge(h.th, p, busy, queued))
}

// statusHint is the one-row action hint that trails the status badge: the
// composer's keys at idle, the cancel affordance and queue depth while a turn
// runs.
func statusHint(busy bool, queued int) string {
	if !busy {
		return "enter sends · alt+enter newline · @ file · ! shell · ? or /help for commands · ctrl+d quits"
	}
	line := "working... esc or ctrl+c cancels"
	if queued > 0 {
		line += fmt.Sprintf(" · %d queued", queued)
	}
	return line
}

// shutdown cancels any in-flight turn, drops the queue, and waits for the
// turn goroutine, so the caller can hand the terminal back and run the
// session's closing work on stdout.
func (h *sessionHost) shutdown() {
	h.mu.Lock()
	h.quitting = true
	h.queue = nil
	cancel := h.cancel
	h.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	h.turns.Wait()
}

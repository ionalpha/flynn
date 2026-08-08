package main

import (
	"context"
	"os"
	"strings"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/internal/tui/editor"
	"github.com/ionalpha/flynn/internal/tui/input"
	tuiterm "github.com/ionalpha/flynn/internal/tui/term"
	"github.com/ionalpha/flynn/internal/tui/theme"
	"github.com/ionalpha/flynn/mission"
)

// onEsc handles the Escape key: an open governance panel closes first, so Escape is the
// dismiss key for the overlay before it is the interrupt for a turn. With the panel
// closed it falls through to interrupt, cancelling the in-flight turn. Ctrl+C stays the
// unconditional cancel, so a user can still stop a turn while reading the panel.
func (h *sessionHost) onEsc() {
	if h.panel.isOpen() {
		h.panel.close()
		h.pokeLive()
		return
	}
	h.interrupt()
}

// interrupt cancels the in-flight turn, if any; the session survives and the
// composer stays live. At idle it does nothing.
func (h *sessionHost) interrupt() {
	h.mu.Lock()
	cancel := h.cancel
	h.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// key claims the session-level keys the editor leaves alone: Ctrl+C during a
// turn cancels the turn (idle Ctrl+C stays with the shell: clear the prompt,
// then quit), Ctrl+D at an empty idle prompt ends the session, matching the
// readline EOF convention the line interface follows, and Ctrl+G hands the
// draft to the user's editor when the session wired a handoff.
func (h *sessionHost) key(k input.Key) bool {
	if k.Mods != input.ModCtrl {
		return false
	}
	h.mu.Lock()
	busy := h.busy
	h.mu.Unlock()
	switch k.Code {
	case 'c':
		if busy {
			h.interrupt()
			return true
		}
	case 'd':
		if !busy {
			h.ui.Quit()
			return true
		}
	case 'g':
		if h.edit != nil {
			h.externalEdit()
			return true
		}
	case 'o':
		h.panel.toggle()
		h.pokeLive()
		return true
	case 'v':
		if h.clip != nil {
			h.paste()
			return true
		}
	}
	return false
}

// paste lands a clipboard image in the composer as an image chip, the action
// behind Ctrl+V and the /paste command. Reading the clipboard happens only
// here, at the user's explicit keystroke, never on the agent's behalf. When
// the clipboard holds no image (or is unavailable on this host), it says so in
// one line rather than failing, so a stray Ctrl+V is harmless.
func (h *sessionHost) paste() {
	if h.clip == nil {
		return
	}
	data, ok := h.clip.Image()
	if !ok {
		h.ui.Append(h.th.Render(theme.Status, "  no image on the clipboard"))
		return
	}
	h.ui.PasteImage(editor.Attachment{MediaType: tuiterm.ImagePNG, Data: data})
}

// externalEdit hands the draft to the user's editor and puts the result
// back in the composer. It runs on the event loop goroutine: the shell is
// suspended for the editor's lifetime and repaints itself after. A streaming
// turn keeps running; its output accumulates and lands after the repaint.
func (h *sessionHost) externalEdit() {
	initial := h.ui.Draft()
	var text string
	var err error
	h.ui.Suspend(func() { text, err = h.edit(initial) })
	if err != nil {
		h.ui.Append(h.th.Render(theme.Rejected, "  editor: "+err.Error()))
		return
	}
	h.ui.SetDraft(strings.TrimRight(text, "\r\n"))
}

// editExternal runs the user's editor over initial through a temp file and
// returns the edited text. release hands the terminal back to the user
// (cooked mode, shell modes off) and returns the reacquire that reverses
// it; reacquire runs even when the editor fails, so the shell always gets
// its terminal back.
func editExternal(initial string, release func() (func() error, error)) (string, error) {
	f, err := os.CreateTemp("", "flynn-prompt-*.md")
	if err != nil {
		return "", err
	}
	path := f.Name()
	defer func() { _ = os.Remove(path) }()
	_, werr := f.WriteString(initial)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return "", werr
	}

	reacquire, err := release()
	if err != nil {
		return "", err
	}
	runErr := tuiterm.RunAttached(append(tuiterm.EditorCommand(), path))
	if err := reacquire(); err != nil {
		return "", err
	}
	if runErr != nil {
		return "", runErr
	}
	edited, err := os.ReadFile(path) //nolint:gosec // reads back the temp file created above
	if err != nil {
		return "", err
	}
	return string(edited), nil
}

// promptApproval drives the interactive approval overlay for one paused action
// and blocks the run goroutine on the operator's decision. It arms the prompt,
// installs it as the shell's modal input capture so the operator's keys reach it
// ahead of the composer, repaints the countdown each second, and resolves when
// the operator allows or denies. A context deadline (the grace period the waist
// set) or cancellation resolves fail-closed: it returns the context error, which
// the waist treats as a decline, so a paused action is never admitted on a
// missing decision. It runs on the turn goroutine, serial with a turn's own
// events, so the prompt is the only thing the operator is deciding on.
func (h *sessionHost) promptApproval(ctx context.Context, req mission.ApprovalRequest) (mission.ApprovalDecision, error) {
	var timing clock.Timing = clock.System{}
	if h.timing != nil {
		timing = h.timing
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		grace := req.Grace
		if grace <= 0 {
			grace = defaultPromptGrace
		}
		deadline = timing.Now().Add(grace)
	}

	done := h.approval.open(req, deadline, timing)
	h.ui.SetCapture(h.approval.handle)
	h.pokeLive()
	defer func() {
		h.approval.clear()
		h.ui.SetCapture(nil)
		h.pokeLive()
	}()

	// Repaint the countdown once a second so the operator watches the grace period
	// tick down; the timer only pokes a repaint, the remaining seconds are computed
	// from the deadline at render time.
	timer := timing.NewTimer(time.Second)
	defer timer.Stop()
	for {
		select {
		case dec := <-done:
			return dec, nil
		case <-timer.C():
			h.pokeLive()
			timer.Reset(time.Second)
		case <-ctx.Done():
			return mission.ApprovalDecision{}, ctx.Err()
		}
	}
}

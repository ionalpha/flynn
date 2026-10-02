package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/ionalpha/flynn/internal/tui/editor"
	"github.com/ionalpha/flynn/internal/tui/theme"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/sandbox"
)

// submit handles one submitted prompt on the shell's event loop: exit
// commands quit, a prompt during a running turn queues behind it, and an
// idle prompt starts its turn immediately.
func (h *sessionHost) submit(text string, images []editor.Attachment) {
	text = strings.TrimSpace(text)
	imgs := toImages(images)
	// /paste is the fallback for terminals that swallow Ctrl+V: it lands a
	// clipboard image in the composer instead of running as a prompt. It only
	// takes over a bare "/paste" with nothing attached, so it never eats a real
	// turn.
	if text == "/paste" && len(imgs) == 0 {
		h.paste()
		return
	}
	if text == "" && len(imgs) == 0 {
		return
	}
	if isExit(text) {
		h.ui.Quit()
		return
	}
	turn := queuedTurn{text: text, images: imgs}
	h.mu.Lock()
	if h.quitting {
		h.mu.Unlock()
		return
	}
	if h.busy {
		h.queue = append(h.queue, turn)
		h.mu.Unlock()
		h.refreshStatus()
		return
	}
	h.busy = true
	h.mu.Unlock()
	h.start(turn)
}

// toImages converts the composer's UI-layer attachments to the model port's
// image type at the one point where a submitted prompt becomes a turn, so
// nothing downstream carries the editor type.
func toImages(atts []editor.Attachment) []llm.Image {
	if len(atts) == 0 {
		return nil
	}
	out := make([]llm.Image, len(atts))
	for i, a := range atts {
		out[i] = llm.Image{MediaType: a.MediaType, Data: a.Data}
	}
	return out
}

// promptEcho is the scrollback echo of a submitted prompt: the text, with a
// trailing count when the turn carried images, so an image-only turn still
// shows in the transcript instead of echoing a blank line.
func promptEcho(t queuedTurn) string {
	if len(t.images) == 0 {
		return t.text
	}
	tag := fmt.Sprintf("[%d image]", len(t.images))
	if len(t.images) != 1 {
		tag = fmt.Sprintf("[%d images]", len(t.images))
	}
	if t.text == "" {
		return tag
	}
	return t.text + " " + tag
}

// start launches text as one turn on its own goroutine. The prompt is echoed
// into the scrollback, the turn's rendered lines follow it as they arrive,
// and when the turn ends the next queued prompt (if any) starts. A "!"
// prompt starts a shell command instead of a model turn; the branch lives
// here so queued shell commands and prompts run in submission order. The
// caller has already marked the host busy.
func (h *sessionHost) start(t queuedTurn) {
	if cmd, ok := strings.CutPrefix(t.text, "!"); ok {
		h.startShell(strings.TrimSpace(cmd))
		return
	}
	// A slash command runs as a recorded command rather than a model turn. Which lines
	// are commands is sessionCommands, the same table the line interface dispatches
	// through and the same one /help lists, so the two interfaces cannot come to
	// understand different sets of commands.
	if cmd, arg, ok := lookupCommand(t.text); ok {
		h.startRecord(func(ctx context.Context) { cmd.tui(h, ctx, arg) })
		return
	}
	turnCtx, cancel := context.WithCancel(h.ctx)
	h.mu.Lock()
	h.cancel = cancel
	h.mu.Unlock()
	h.refreshStatus()

	h.echoPrompt(promptEcho(t))
	// The turn driver taps every session event through the observer: the shell
	// renders the typed stream itself (transcript, governance, badge), so the
	// driver's flat text goes nowhere.
	h.s.out = io.Discard
	h.s.observer = h.onEvent
	h.rendered = false
	h.live.set("thinking...")
	h.pokeLive()

	h.turns.Add(1)
	go func() {
		defer h.turns.Done()
		_, err := h.s.runTurn(turnCtx, t.text, t.images, nil)
		cancel()
		h.live.set("")
		h.pokeLive()
		switch {
		case errors.Is(err, context.Canceled):
			h.ui.Append("  (turn cancelled)")
		case err != nil:
			h.ui.Append("  error: " + err.Error())
		case !h.rendered:
			// The turn finished cleanly but produced nothing to show (a weak model
			// that emitted only whitespace), so say so rather than leave a blank.
			h.ui.Append(h.th.Render(theme.Muted, "  (no response)"))
		}
		h.next()
	}()
}

// startShell runs one composer shell command as the current turn: echoed
// into the scrollback under the shell marker, executed through the confined
// sandbox, its output committed below the echo. It holds the same turn slot
// a prompt does, so Escape and Ctrl+C cancel a running command and anything
// queued behind it waits. The caller has already marked the host busy.
func (h *sessionHost) startShell(cmdLine string) {
	turnCtx, cancel := context.WithCancel(h.ctx)
	h.mu.Lock()
	h.cancel = cancel
	h.mu.Unlock()
	h.refreshStatus()

	h.ui.Append("", h.th.Render(theme.UserPrefix, "! ")+h.th.Render(theme.UserText, cmdLine), "")
	h.turns.Add(1)
	go func() {
		defer h.turns.Done()
		h.runShell(turnCtx, cmdLine)
		cancel()
		h.next()
	}()
}

// runShell executes one shell-mode command and commits its outcome to the
// scrollback: the command's combined output, then its exit code when it is
// not zero. Cancellation reads as a cancelled command, not an error.
func (h *sessionHost) runShell(ctx context.Context, cmdLine string) {
	if cmdLine == "" {
		h.ui.Append(h.th.Render(theme.Status, "  usage: !<command> runs a shell command in "+h.s.cwd))
		return
	}
	if h.run == nil {
		msg := "no sandbox at the working directory"
		if h.runErr != nil {
			msg = h.runErr.Error()
		}
		h.ui.Append("  shell mode unavailable: " + msg)
		return
	}
	res, err := h.run.Exec(ctx, sandbox.Command{Line: cmdLine})
	switch {
	case errors.Is(ctx.Err(), context.Canceled):
		h.ui.Append("  (command cancelled)")
		return
	case err != nil:
		h.ui.Append("  error: " + err.Error())
		return
	}
	var lines []string
	if out := strings.TrimRight(res.Output, "\n"); out != "" {
		for _, line := range strings.Split(out, "\n") {
			lines = append(lines, h.th.Render(theme.ToolOutput, "  "+strings.TrimRight(line, "\r")))
		}
	}
	if res.ExitCode != 0 {
		lines = append(lines, h.th.Render(theme.Rejected, fmt.Sprintf("  exit %d", res.ExitCode)))
	}
	if len(lines) > 0 {
		h.ui.Append(lines...)
	}
}

// startRecord runs one in-process record command (seal or verify) as the current turn.
// It holds the same turn slot a prompt does, so it runs serially after any in-flight
// turn and never reads the run's stream while a turn is still appending to it. The
// caller has already marked the host busy.
func (h *sessionHost) startRecord(run func(context.Context)) {
	turnCtx, cancel := context.WithCancel(h.ctx)
	h.mu.Lock()
	h.cancel = cancel
	h.mu.Unlock()
	h.refreshStatus()

	h.turns.Add(1)
	go func() {
		defer h.turns.Done()
		run(turnCtx)
		cancel()
		h.next()
	}()
}

// next ends the finished turn's ownership of the shell: it starts the next
// queued prompt if one is waiting, or returns the session to idle. A closing
// shell drops the queue instead, so shutdown never races a fresh turn.
func (h *sessionHost) next() {
	h.mu.Lock()
	h.cancel = nil
	if h.quitting {
		h.queue = nil
		h.busy = false
		h.mu.Unlock()
		return
	}
	if len(h.queue) > 0 {
		turn := h.queue[0]
		h.queue = h.queue[1:]
		h.mu.Unlock()
		h.start(turn)
		return
	}
	h.busy = false
	h.mu.Unlock()
	h.refreshStatus()
}

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/internal/tui/app"
	"github.com/ionalpha/flynn/internal/tui/input"
	"github.com/ionalpha/flynn/internal/tui/screen"
	tuiterm "github.com/ionalpha/flynn/internal/tui/term"
	"github.com/ionalpha/flynn/internal/tui/theme"
)

// resizePoll is how often the shell samples the terminal size. Polling is the
// portable resize signal: it needs no SIGWINCH, so the same loop works on
// Windows consoles and Unix terminals alike.
const resizePoll = 250 * time.Millisecond

// runInteractiveTUI runs the full-screen interactive session on the
// scrollback-native shell: finalized transcript lines are committed to the
// terminal's own scrollback (selectable, searchable, wrapped by the terminal),
// the in-flight turn renders in a live region, and the composer stays at the
// bottom. It reuses the whole turn driver (recall, reopen, streaming,
// cancellation, learning) by pointing the session's output at the shell, so
// the agent behaviour is identical to the line-based session; only the
// presentation differs. When the shell exits, output is restored to stdout
// and the session's learning pass runs there.
func runInteractiveTUI(ctx context.Context, s *replSession, seed string) error {
	fd := int(os.Stdin.Fd())
	restore, err := tuiterm.MakeRaw(fd)
	if err != nil {
		// No raw mode means no shell; the line interface still works.
		return s.runLineMode(ctx, s.cwd)
	}
	modes := tuiterm.Options{BracketedPaste: true, KittyKeyboard: true, HideCursor: true}
	if err := tuiterm.Setup(os.Stdout, modes); err != nil {
		_ = restore()
		return err
	}

	width, height, _ := tuiterm.Size(fd) // zero on error selects the shell's defaults
	a, host := newSessionShell(ctx, s, os.Stdin, os.Stdout, width, height)
	watcher := tuiterm.WatchResize(clock.System{}, resizePoll, func() (int, int, error) {
		return tuiterm.Size(fd)
	}, a.Resize)

	host.greet(seed)
	runErr := a.Run()

	watcher.Stop()
	host.shutdown()
	if err := tuiterm.Teardown(os.Stdout, modes); err != nil && runErr == nil {
		runErr = err
	}
	if err := restore(); err != nil && runErr == nil {
		runErr = err
	}

	s.out = &syncWriter{w: os.Stdout}
	if ferr := s.finish(ctx); ferr != nil && runErr == nil {
		runErr = ferr
	}
	return runErr
}

// newSessionShell wires one replSession to a shell over the given terminal
// streams. It is split from runInteractiveTUI so tests can drive the exact
// production wiring over pipes, with no terminal required.
func newSessionShell(ctx context.Context, s *replSession, in io.Reader, out io.Writer, width, height int) (*app.App, *sessionHost) {
	host := &sessionHost{ctx: ctx, s: s, th: theme.Default()}
	a := app.New(app.Config{
		Input:       in,
		Output:      out,
		Width:       width,
		Height:      height,
		Theme:       host.th,
		Placeholder: "Send a message",
		OnSubmit:    host.submit,
		OnEsc:       host.interrupt,
		OnKey:       host.key,
	})
	host.ui = a
	return a, host
}

// shellUI is the slice of the shell the session host drives. An interface so
// tests can observe the host through a recording fake.
type shellUI interface {
	Append(lines ...string)
	SetLive(c screen.Component)
	SetStatus(line string)
	Quit()
}

// sessionHost owns the session policy on top of the shell's mechanics: it
// echoes and queues submitted prompts, drives each one as a turn of the
// replSession on its own goroutine, routes the turn's rendered output into
// the scrollback, and maps Escape and Ctrl+C onto cancelling the in-flight
// turn. Turns are strictly serial; a prompt submitted while one runs waits
// its turn in the queue.
type sessionHost struct {
	ctx context.Context
	s   *replSession
	ui  shellUI
	th  *theme.Theme

	mu       sync.Mutex
	busy     bool
	queue    []string
	cancel   context.CancelFunc
	quitting bool
	turns    sync.WaitGroup
}

// greet writes the session banner and, for a resumed run, its rendered
// history, so the conversation's context is in the scrollback from the start.
func (h *sessionHost) greet(seed string) {
	h.ui.Append(h.th.Render(theme.Status, "flynn interactive session in "+h.s.cwd))
	if seed != "" {
		h.ui.Append(strings.Split(strings.TrimRight(seed, "\n"), "\n")...)
	}
	h.refreshStatus()
}

// submit handles one submitted prompt on the shell's event loop: exit
// commands quit, a prompt during a running turn queues behind it, and an
// idle prompt starts its turn immediately.
func (h *sessionHost) submit(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	if isExit(text) {
		h.ui.Quit()
		return
	}
	h.mu.Lock()
	if h.quitting {
		h.mu.Unlock()
		return
	}
	if h.busy {
		h.queue = append(h.queue, text)
		h.mu.Unlock()
		h.refreshStatus()
		return
	}
	h.busy = true
	h.mu.Unlock()
	h.start(text)
}

// start launches text as one turn on its own goroutine. The prompt is echoed
// into the scrollback, the turn's rendered lines follow it as they arrive,
// and when the turn ends the next queued prompt (if any) starts. The caller
// has already marked the host busy.
func (h *sessionHost) start(text string) {
	turnCtx, cancel := context.WithCancel(h.ctx)
	h.mu.Lock()
	h.cancel = cancel
	h.mu.Unlock()
	h.refreshStatus()

	h.ui.Append("", h.th.Render(theme.UserPrefix, "> ")+h.th.Render(theme.UserText, text))
	sink := &turnSink{ui: h.ui}
	h.s.out = sink
	h.ui.SetLive(sink)

	h.turns.Add(1)
	go func() {
		defer h.turns.Done()
		_, err := h.s.runTurn(turnCtx, text, nil)
		cancel()
		sink.finish()
		h.ui.SetLive(nil)
		switch {
		case errors.Is(err, context.Canceled):
			h.ui.Append("  (turn cancelled)")
		case err != nil:
			h.ui.Append("  error: " + err.Error())
		}
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
		text := h.queue[0]
		h.queue = h.queue[1:]
		h.mu.Unlock()
		h.start(text)
		return
	}
	h.busy = false
	h.mu.Unlock()
	h.refreshStatus()
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
// then quit), and Ctrl+D at an empty idle prompt ends the session, matching
// the readline EOF convention the line interface follows.
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
	}
	return false
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

// refreshStatus rewrites the status line from the host's current state.
func (h *sessionHost) refreshStatus() {
	h.mu.Lock()
	busy, queued := h.busy, len(h.queue)
	h.mu.Unlock()
	h.ui.SetStatus(statusLine(busy, queued))
}

// statusLine is the one-row hint between the live output and the composer.
func statusLine(busy bool, queued int) string {
	if !busy {
		return "enter sends · alt+enter or ctrl+j newline · up/down history · ctrl+d quits"
	}
	line := "working... esc or ctrl+c cancels"
	if queued > 0 {
		line += fmt.Sprintf(" · %d queued", queued)
	}
	return line
}

// turnSink routes a turn's rendered output into the shell. Every completed
// line is committed to the scrollback as it arrives; the trailing partial
// line (output not yet ended by a newline) renders as the live region until
// it completes, so mid-line progress is visible without ever committing a
// line twice.
type turnSink struct {
	ui shellUI

	mu  sync.Mutex
	buf []byte
}

// Write splits the stream on newlines. It never calls into the shell while
// holding the sink's lock: the shell's paint path calls Render under its own
// lock, so nesting the two would invert the lock order.
func (t *turnSink) Write(p []byte) (int, error) {
	t.mu.Lock()
	t.buf = append(t.buf, p...)
	var lines []string
	for {
		i := bytes.IndexByte(t.buf, '\n')
		if i < 0 {
			break
		}
		lines = append(lines, strings.TrimRight(string(t.buf[:i]), "\r"))
		t.buf = t.buf[i+1:]
	}
	t.mu.Unlock()
	if len(lines) > 0 {
		t.ui.Append(lines...)
	}
	return len(p), nil
}

// finish commits any trailing partial line, so output not ended by a newline
// still lands in the transcript when the turn ends.
func (t *turnSink) finish() {
	t.mu.Lock()
	rest := string(t.buf)
	t.buf = nil
	t.mu.Unlock()
	if strings.TrimSpace(rest) != "" {
		t.ui.Append(rest)
	}
}

// Render shows the pending partial line in the live region, wrapped to the
// terminal width.
func (t *turnSink) Render(width int) []string {
	t.mu.Lock()
	rest := string(t.buf)
	t.mu.Unlock()
	if strings.TrimSpace(rest) == "" {
		return nil
	}
	return strings.Split(screen.Wrap(rest, width), "\n")
}

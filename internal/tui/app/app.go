// Package app is the interactive session shell: one event loop composing the
// input reader, the composer editor, and the screen painter into a running
// terminal application. The shell owns mechanics, not policy: it decodes
// keystrokes into editor actions, keeps prompt history, coalesces repaints
// into frames, and hands every session-level intent (a submitted prompt,
// Escape, a key nothing claimed) to the caller through hooks. What a prompt
// means, what streams back, and how output is styled belong to the host.
//
// Concurrency follows one rule: all mutable state sits behind a single mutex,
// hooks are invoked with the mutex released, and every mutator only records
// state and requests a frame. Painting happens solely on the frame
// scheduler's goroutine, so the painter, which is not concurrency safe, has
// exactly one caller for the shell's whole life.
//
// The shell is pure with respect to the terminal: it reads decoded events
// from any io.Reader and writes frames to any io.Writer, takes its size by
// value, and takes time through the injected clock. Raw mode, terminal
// modes, and resize watching are the term package's job, wired by the caller.
package app

import (
	"io"
	"strings"
	"sync"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/internal/tui/editor"
	"github.com/ionalpha/flynn/internal/tui/input"
	"github.com/ionalpha/flynn/internal/tui/screen"
	"github.com/ionalpha/flynn/internal/tui/theme"
)

// Config wires the shell to its terminal and its host. Input and Output are
// required; every other field has a working default.
type Config struct {
	// Input is the terminal's raw byte stream (stdin in raw mode). The shell
	// stops when it reaches EOF or when Quit is called; closing Input is the
	// caller's responsibility and is what unblocks a pending read.
	Input io.Reader
	// Output receives frames (stdout). Writes are single buffered frames
	// wrapped in synchronized output.
	Output io.Writer
	// Timing is the shell's source of time (frame pacing, the escape-key
	// delay). Nil means the system clock.
	Timing clock.Timing
	// Width and Height are the terminal's size in cells at startup; resizes
	// arrive later through Resize. Zero values default to 80 by 24.
	Width, Height int
	// Theme styles the shell's own chrome (the prompt gutter, the status
	// line, the placeholder). Nil means the default theme.
	Theme *theme.Theme
	// FrameInterval is the minimum time between repaints. Zero means about
	// sixty frames per second.
	FrameInterval time.Duration
	// EscDelay is how long a lone Escape byte may dangle before it resolves
	// as the Escape key rather than the start of a sequence. Zero means 50ms.
	EscDelay time.Duration
	// Placeholder is the hint shown in the empty composer.
	Placeholder string
	// Keymap is the composer's key bindings. Nil means the default map;
	// build a custom one with editor.LoadKeymap.
	Keymap editor.Keymap
	// AltScreen selects the alternate-screen renderer instead of the default
	// inline one. The inline renderer commits the transcript to the terminal's
	// own scrollback and is the right choice almost everywhere; the alternate
	// screen is the fallback for emulators where inline scroll-region insertion
	// is unsafe (Zellij-class multiplexers). The caller enters and leaves the
	// alternate screen around Run (through the term package) when this is set.
	AltScreen bool

	// OnSubmit receives each submitted prompt: the prompt text and the images
	// attached to it (nil when there are none), in the order their chips
	// appeared. It runs on the event loop goroutine with no locks held; long
	// work belongs on the host's own goroutine, which reports back through
	// Append, SetLive, and SetStatus.
	OnSubmit func(text string, images []editor.Attachment)
	// OnEsc fires when Escape is pressed. The host owns what Escape means:
	// interrupt the in-flight turn, dismiss a panel, nothing.
	OnEsc func()
	// OnKey receives keys neither the editor nor a named hook claimed.
	// Return true to consume the key; unconsumed keys fall through to the
	// shell's defaults (Ctrl+C).
	OnKey func(k input.Key) bool
	// Completer supplies @-completion candidates. The shell owns the popup
	// (tracking the token at the cursor, navigation, accept, dismiss); the
	// host owns the universe being completed over. Nil disables completion.
	// Both methods run on the event loop goroutine with no locks held, so
	// Complete must be fast; slow indexing belongs behind a host-built cache.
	Completer Completer
	// Commands supplies completion for a slash-command line. A slashed line has no
	// trigger token the way an @-mention does, so command and argument completion look
	// at the whole composer line instead: given that line, Suggest returns the
	// candidates to replace it with. Nil disables slash-command completion.
	Commands CommandCompleter
	// Marker maps the composer's current content to its first-row gutter
	// marker, so the host can surface an input mode the content selects
	// ("! " while the prompt is a shell command). Empty selects the default
	// prompt marker; a non-empty marker is fitted to the gutter's width. It
	// runs on the paint goroutine under the shell's lock, so it must be a
	// fast pure function of its argument. Nil always uses the default.
	Marker func(content string) string
}

// Completer is the host side of @-completion. Complete returns the
// candidates for a query, best first, at most a menu's worth; the query may
// be empty (the trigger was just typed, ask for the unfiltered universe).
// Accepted reports the candidate the user chose, so the host can rank
// recent picks higher.
type Completer interface {
	Complete(query string) []string
	Accepted(item string)
}

// CommandCompleter is the host side of slash-command completion. Suggest returns the
// candidates for the current composer line, best first and at most a menu's worth, or
// nil when nothing applies (the line is not a command, or it is already complete). Each
// candidate carries the text shown in the menu and the whole line to set when it is
// chosen. It runs on the event loop with no lock held, so it must be fast.
type CommandCompleter interface {
	Suggest(line string) []CommandCandidate
}

// CommandCandidate is one slash-command completion: Show is the menu label, Apply is
// the whole composer line the shell sets when the candidate is chosen.
type CommandCandidate struct {
	Show  string
	Apply string
}

const (
	defaultWidth         = 80
	defaultHeight        = 24
	defaultFrameInterval = 16 * time.Millisecond
	defaultEscDelay      = 50 * time.Millisecond
)

// App is the running shell. Create one with New, drive it with Run, and feed
// it from any goroutine through Append, SetLive, SetStatus, and Resize.
type App struct {
	cfg   Config
	sched *screen.Scheduler

	quitOnce sync.Once
	quit     chan struct{}

	mu        sync.Mutex
	painter   screen.Surface
	editor    editor.Editor
	history   history
	live      screen.Component
	status    string
	finalized []string
	width     int
	height    int
	resized   bool
	menu      menu
	accepted  []string
	reader    *input.Reader
	suspended bool
	// capture, when set, takes first claim on every decoded input event before
	// the completion menu and the composer: a modal overlay (an approval prompt)
	// installs it to own the keyboard while it is up, and clears it when it
	// resolves. It returns whether it consumed the event; an unconsumed event
	// falls through to the normal editor path, so the modal can let keys it does
	// not use pass. Nil is the default: no modal, input flows to the composer.
	capture func(input.Event) bool
}

// New builds the shell and starts its frame scheduler. Run must be called
// exactly once to drive input and to release the scheduler on the way out.
func New(cfg Config) *App {
	if cfg.Timing == nil {
		cfg.Timing = clock.System{}
	}
	if cfg.Theme == nil {
		cfg.Theme = theme.Default()
	}
	if cfg.FrameInterval <= 0 {
		cfg.FrameInterval = defaultFrameInterval
	}
	if cfg.EscDelay <= 0 {
		cfg.EscDelay = defaultEscDelay
	}
	if cfg.Width < 1 {
		cfg.Width = defaultWidth
	}
	if cfg.Height < 2 {
		cfg.Height = defaultHeight
	}
	a := &App{
		cfg:     cfg,
		quit:    make(chan struct{}),
		painter: newSurface(cfg),
		width:   cfg.Width,
		height:  cfg.Height,
	}
	a.editor.SetKeymap(cfg.Keymap)
	a.sched = screen.NewScheduler(cfg.Timing, cfg.FrameInterval, a.paint)
	return a
}

// newSurface builds the render target for the shell: the alternate-screen
// painter when the caller asked for it, the inline scrollback-native painter
// otherwise. Both satisfy screen.Surface, so the rest of the shell drives
// either without caring which it holds.
func newSurface(cfg Config) screen.Surface {
	if cfg.AltScreen {
		return screen.NewAltPainter(cfg.Output, cfg.Width, cfg.Height)
	}
	return screen.NewPainter(cfg.Output, cfg.Width, cfg.Height)
}

// Run paints the first frame, then consumes input events until Quit is
// called or Input reaches EOF. On the way out it stops the scheduler, flushes
// any pending finalized lines, leaves the last frame in the terminal's
// scrollback with the cursor on a fresh line below it, and returns the first
// write error the painter hit, if any.
func (a *App) Run() error {
	reader := input.NewReader(a.cfg.Input, a.cfg.Timing, a.cfg.EscDelay)
	a.mu.Lock()
	a.reader = reader
	a.mu.Unlock()
	a.sched.Request()
loop:
	for {
		select {
		case <-a.quit:
			break loop
		case ev, ok := <-reader.Events():
			if !ok {
				break loop
			}
			a.handle(ev)
		}
	}
	// Stop the scheduler first so the direct paint below cannot race it;
	// after Stop returns, this goroutine is the painter's only caller.
	a.sched.Stop()
	a.paint()
	a.mu.Lock()
	a.painter.Close()
	err := a.painter.Err()
	a.mu.Unlock()
	reader.Stop()
	return err
}

// Quit stops Run from any goroutine. Safe to call more than once.
func (a *App) Quit() {
	a.quitOnce.Do(func() { close(a.quit) })
}

// Append commits finalized lines to the terminal's scrollback above the live
// region on the next frame. The lines are styled by the caller and become
// ordinary terminal output: never repainted, wrapped by the terminal itself,
// selectable and searchable like any other program's output.
func (a *App) Append(lines ...string) {
	if len(lines) == 0 {
		return
	}
	a.mu.Lock()
	a.finalized = append(a.finalized, lines...)
	a.mu.Unlock()
	a.sched.Request()
}

// SetLive installs the component rendered above the status line and the
// composer: the in-flight output being streamed, repainted in place each
// frame. Nil clears it.
func (a *App) SetLive(c screen.Component) {
	a.mu.Lock()
	a.live = c
	a.mu.Unlock()
	a.sched.Request()
}

// SetCapture installs a modal input handler that takes first claim on every
// decoded event, ahead of the completion menu and the composer, so an overlay
// (an approval prompt) can own the keyboard while it is up. The handler reports
// whether it consumed the event; an unconsumed event falls through to the normal
// editor path. Passing nil clears the modal and returns input to the composer.
// It is safe to call from any goroutine.
func (a *App) SetCapture(fn func(input.Event) bool) {
	a.mu.Lock()
	a.capture = fn
	a.mu.Unlock()
	a.sched.Request()
}

// SetStatus sets the one-row status line between the live output and the
// composer. Empty hides it.
func (a *App) SetStatus(line string) {
	a.mu.Lock()
	a.status = line
	a.mu.Unlock()
	a.sched.Request()
}

// Draft returns the composer's current content.
func (a *App) Draft() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.editor.Content()
}

// SetDraft replaces the composer's content, cursor at the end.
func (a *App) SetDraft(text string) {
	a.mu.Lock()
	a.setContentLocked(text)
	a.mu.Unlock()
	a.sched.Request()
}

// PasteImage inserts an image chip at the cursor and repaints. It is how the
// host lands a clipboard image in the composer (Ctrl+V, or the /paste
// fallback): the chip carries the image out of line, and the bytes surface on
// the next submit through Attachments. Empty data is ignored.
func (a *App) PasteImage(att editor.Attachment) {
	a.mu.Lock()
	a.editor.InsertImage(att)
	a.mu.Unlock()
	a.sched.Request()
}

// cursorQuery is the poke written while pausing the reader: a cursor
// position query the terminal answers on the input stream, completing a
// blocked read so the read goroutine can park. The answer is discarded by
// the paused reader (or decodes to an ignored Unknown event if it arrives
// after resume).
const cursorQuery = "\x1b[6n"

// Suspend hands the terminal to run for its duration: painting stops, the
// live region is cleared so run's output starts below the scrollback, and
// the input reader is parked off the input stream so run's process can own
// it exclusively. After run returns, the shell reclaims the input and
// repaints from scratch. The caller owns the terminal's modes: leave raw
// mode and the shell's terminal modes inside run before starting a child
// process, and restore them before returning. Suspend must be called from a
// hook (the event loop goroutine); before Run, or while already suspended,
// it does nothing.
func (a *App) Suspend(run func()) {
	a.mu.Lock()
	reader := a.reader
	if reader == nil || a.suspended {
		a.mu.Unlock()
		return
	}
	a.suspended = true
	// Direct painter calls are safe here: every painter call sits behind
	// this mutex, and with suspended set the scheduler's paint is a no-op,
	// so the poke below is the only terminal writer until resume.
	a.painter.Repaint(nil)
	out := a.cfg.Output
	a.mu.Unlock()

	parked := reader.Pause(func() { _, _ = out.Write([]byte(cursorQuery)) })
	// Drain stale events while waiting so a full event buffer can never
	// wedge the pump between Pause and the park; anything typed after the
	// suspending keystroke was meant for run's process, not the composer.
	for waiting := true; waiting; {
		select {
		case <-parked:
			waiting = false
		case _, ok := <-reader.Events():
			if !ok {
				waiting = false
			}
		}
	}

	run()

	reader.Resume()
	a.mu.Lock()
	// run's process may have left the cursor mid-line; return to column
	// zero so the repainted frame starts on a clean left edge.
	_, _ = out.Write([]byte("\r"))
	a.suspended = false
	a.resized = true
	a.mu.Unlock()
	a.sched.Request()
}

// Resize records a new terminal size; the next frame re-renders every
// component at the new width and repaints from scratch, since the terminal
// rewrapped the old rows and the painter's diff base is stale.
func (a *App) Resize(width, height int) {
	a.mu.Lock()
	a.width, a.height = width, height
	a.resized = true
	a.mu.Unlock()
	a.sched.Request()
}

// Width returns the terminal's current width in cells. A host that renders its
// own content to fit the terminal (wrapping markdown, laying out a transcript)
// reads it here so its output tracks resizes, which arrive through Resize on the
// event loop goroutine while the host renders on its own.
func (a *App) Width() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.width
}

// handle consumes one decoded input event on the event loop goroutine. An
// open completion menu gets first claim on the navigation keys; everything
// else flows through the editor, and afterwards the menu re-derives itself
// from the composer's state.
func (a *App) handle(ev input.Event) {
	// A modal overlay gets first claim on every event, ahead of the menu and the
	// composer. It runs with no lock held (it reaches back into the shell to
	// repaint and to resolve), and a consumed event ends the frame here.
	a.mu.Lock()
	capture := a.capture
	a.mu.Unlock()
	if capture != nil && capture(ev) {
		a.sched.Request()
		return
	}
	if k, isKey := ev.(input.Key); isKey && a.menuKey(k) {
		a.notifyAccepted()
		a.sched.Request()
		return
	}
	var after func()
	a.mu.Lock()
	action := a.editor.Handle(ev)
	switch action {
	case editor.ActionSubmit:
		text := a.editor.Content()
		images := a.editor.Attachments()
		// An image-only prompt (chips, no prose) is a real turn; submit when
		// there is text or at least one attachment.
		if strings.TrimSpace(text) != "" || len(images) > 0 {
			a.editor.Clear()
			if strings.TrimSpace(text) != "" {
				a.history.add(text)
			}
			if h := a.cfg.OnSubmit; h != nil {
				after = func() { h(text, images) }
			}
		}
	case editor.ActionEsc:
		after = a.cfg.OnEsc
	case editor.ActionHistoryPrev:
		if text, ok := a.history.prev(a.editor.Content()); ok {
			a.setContentLocked(text)
		}
	case editor.ActionHistoryNext:
		if text, ok := a.history.next(); ok {
			a.setContentLocked(text)
		}
	case editor.ActionNone, editor.ActionRedraw, editor.ActionTab:
		// Redraw needs nothing beyond the frame request below; None and Tab
		// fall through to the host's OnKey and the shell's defaults.
	}
	a.mu.Unlock()
	if after != nil {
		after()
	}
	if action == editor.ActionNone || action == editor.ActionTab {
		if k, isKey := ev.(input.Key); isKey {
			a.fallbackKey(k)
		}
	}
	a.refreshMenu()
	a.sched.Request()
}

// fallbackKey handles keys the editor did not claim: first the host's OnKey
// hook, then the shell's defaults. Ctrl+C clears a non-empty prompt; on an
// empty prompt it quits.
func (a *App) fallbackKey(k input.Key) {
	if h := a.cfg.OnKey; h != nil && h(k) {
		return
	}
	if k.Mods == input.ModCtrl && k.Code == 'c' {
		a.mu.Lock()
		empty := a.editor.Empty()
		if !empty {
			a.editor.Clear()
		}
		a.mu.Unlock()
		if empty {
			a.Quit()
		}
	}
}

// setContentLocked replaces the composer's content with a recalled prompt,
// cursor at the end.
func (a *App) setContentLocked(text string) {
	a.editor.Clear()
	a.editor.Insert(text)
}

// paint composes and paints one frame. It runs on the scheduler's goroutine
// while Run is live, and once more on Run's goroutine after the scheduler
// has stopped, for the final flush.
func (a *App) paint() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.suspended {
		return
	}
	resized := a.resized
	a.resized = false
	if resized {
		a.painter.Resize(a.width, a.height)
	}
	frame := a.composeLocked()
	finalized := a.finalized
	a.finalized = nil
	switch {
	case len(finalized) > 0:
		// Insert clears the live region and rewrites the whole frame, so it
		// doubles as the from-scratch repaint a resize needs.
		a.painter.Insert(finalized, frame)
	case resized:
		a.painter.Repaint(frame)
	default:
		a.painter.Paint(frame)
	}
}

// composeLocked renders the live region for this frame: the in-flight
// output, the status line, and the composer, top to bottom. The painter's
// overflow guard keeps the bottom rows when the frame is taller than the
// terminal, so the composer always survives clipping.
func (a *App) composeLocked() []string {
	// A blank row opens the live region so the pinned status and composer read as
	// their own block, set apart from the transcript scrolling above them rather
	// than glued to its last line.
	frame := []string{""}
	if a.live != nil {
		frame = append(frame, a.live.Render(a.width)...)
	}
	if a.status != "" {
		frame = append(frame, a.cfg.Theme.Render(theme.Status, screen.Truncate(a.status, a.width)))
		// A blank row sets the composer apart from the status strip above it, so the
		// input does not read as jammed onto the status line.
		frame = append(frame, "")
	}
	if a.menu.open {
		frame = append(frame, a.menuRowsLocked()...)
	}
	marker := ""
	if a.cfg.Marker != nil {
		marker = a.cfg.Marker(a.editor.Content())
	}
	return append(frame, composerRows(&a.editor, a.cfg.Theme, a.width, a.cfg.Placeholder, marker)...)
}

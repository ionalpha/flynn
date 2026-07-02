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

	// OnSubmit receives each submitted prompt. It runs on the event loop
	// goroutine with no locks held; long work belongs on the host's own
	// goroutine, which reports back through Append, SetLive, and SetStatus.
	OnSubmit func(text string)
	// OnEsc fires when Escape is pressed. The host owns what Escape means:
	// interrupt the in-flight turn, dismiss a panel, nothing.
	OnEsc func()
	// OnKey receives keys neither the editor nor a named hook claimed,
	// including Tab (completion needs the command and file universe the
	// shell has no business knowing). Return true to consume the key;
	// unconsumed keys fall through to the shell's defaults (Ctrl+C).
	OnKey func(k input.Key) bool
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
	painter   *screen.Painter
	editor    editor.Editor
	history   history
	live      screen.Component
	status    string
	finalized []string
	width     int
	height    int
	resized   bool
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
		painter: screen.NewPainter(cfg.Output, cfg.Width, cfg.Height),
		width:   cfg.Width,
		height:  cfg.Height,
	}
	a.sched = screen.NewScheduler(cfg.Timing, cfg.FrameInterval, a.paint)
	return a
}

// Run paints the first frame, then consumes input events until Quit is
// called or Input reaches EOF. On the way out it stops the scheduler, flushes
// any pending finalized lines, leaves the last frame in the terminal's
// scrollback with the cursor on a fresh line below it, and returns the first
// write error the painter hit, if any.
func (a *App) Run() error {
	reader := input.NewReader(a.cfg.Input, a.cfg.Timing, a.cfg.EscDelay)
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

// SetStatus sets the one-row status line between the live output and the
// composer. Empty hides it.
func (a *App) SetStatus(line string) {
	a.mu.Lock()
	a.status = line
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

// handle consumes one decoded input event on the event loop goroutine.
func (a *App) handle(ev input.Event) {
	var after func()
	a.mu.Lock()
	action := a.editor.Handle(ev)
	switch action {
	case editor.ActionSubmit:
		text := a.editor.Content()
		if strings.TrimSpace(text) != "" {
			a.editor.Clear()
			a.history.add(text)
			if h := a.cfg.OnSubmit; h != nil {
				after = func() { h(text) }
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
	var frame []string
	if a.live != nil {
		frame = append(frame, a.live.Render(a.width)...)
	}
	if a.status != "" {
		frame = append(frame, a.cfg.Theme.Render(theme.Status, screen.Truncate(a.status, a.width)))
	}
	return append(frame, composerRows(&a.editor, a.cfg.Theme, a.width, a.cfg.Placeholder)...)
}

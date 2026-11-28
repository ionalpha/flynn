// Package app is the interactive session shell: one event loop composing the
// input reader, the composer editor and the screen painter. It decodes
// keystrokes into editor actions, keeps prompt history, coalesces repaints into
// frames, and hands every session-level intent (a submitted prompt, Escape, an
// unclaimed key) to the caller through hooks. What a prompt means and how output
// is styled belong to the host.
//
// All mutable state sits behind a single mutex, hooks run with it released, and
// every mutator only records state and requests a frame. Painting happens solely
// on the frame scheduler's goroutine, so the painter, which is not safe for
// concurrent use, has one caller for the shell's whole life.
//
// The shell reads events from any io.Reader, writes frames to any io.Writer and
// takes time from the injected clock. Raw mode, terminal modes and resize
// watching belong to the term package, wired by the caller.
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
// required; every other field has a default.
type Config struct {
	// Input is the terminal's raw byte stream. The shell stops at EOF or Quit;
	// closing Input is the caller's job and is what unblocks a pending read.
	Input io.Reader
	// Output receives frames, each one buffered write wrapped in synchronized
	// output.
	Output io.Writer
	// Timing is the source of time. Nil means the system clock.
	Timing clock.Timing
	// Width and Height are the size in cells at startup; later resizes arrive
	// through Resize. Zero values default to 80 by 24.
	Width, Height int
	// Theme styles the shell's own chrome. Nil means the default theme.
	Theme *theme.Theme
	// FrameInterval is the minimum time between repaints. Zero means about 60
	// frames per second.
	FrameInterval time.Duration
	// EscDelay is how long a lone Escape byte may dangle before it resolves as
	// the Escape key rather than the start of a sequence. Zero means 50ms.
	EscDelay time.Duration
	// Placeholder is the hint shown in the empty composer.
	Placeholder string
	// Keymap is the composer's key bindings. Nil means the default map.
	Keymap editor.Keymap
	// AltScreen selects the alternate-screen renderer instead of the inline one,
	// which commits the transcript to the terminal's scrollback. Use it only for
	// emulators where inline scroll-region insertion is unsafe (Zellij-class
	// multiplexers). The caller enters and leaves the alternate screen around Run.
	AltScreen bool

	// OnSubmit receives each submitted prompt and its attached images (nil when
	// none), in chip order. It runs on the event loop goroutine with no locks
	// held; long work belongs on the host's own goroutine, which reports back
	// through Append, SetLive and SetStatus.
	OnSubmit func(text string, images []editor.Attachment)
	// OnEsc fires when Escape is pressed; the host decides what it means.
	OnEsc func()
	// OnKey receives keys neither the editor nor a named hook claimed. Return
	// true to consume the key; the rest fall through to the defaults (Ctrl+C).
	OnKey func(k input.Key) bool
	// Completer supplies @-completion candidates. Nil disables completion. Both
	// methods run on the event loop goroutine with no locks held, so Complete
	// must be fast.
	Completer Completer
	// Commands supplies completion for a slash-command line. Unlike an @-mention
	// it has no trigger token, so Suggest sees the whole composer line. Nil
	// disables it.
	Commands CommandCompleter
	// Marker maps the composer's content to its first-row gutter marker, so the
	// host can show an input mode ("! " while the prompt is a shell command).
	// Empty selects the default marker; a non-empty one is fitted to the gutter
	// width. It runs on the paint goroutine under the shell's lock, so it must be
	// fast and pure. Nil always uses the default.
	Marker func(content string) string
}

// Completer is the host side of @-completion. Complete returns candidates for a
// query, best first, at most a menu's worth; the query may be empty (the
// unfiltered universe). Accepted reports the chosen candidate so the host can
// rank recent picks higher.
type Completer interface {
	Complete(query string) []string
	Accepted(item string)
}

// CommandCompleter is the host side of slash-command completion. Suggest returns
// candidates for the composer line, best first and at most a menu's worth, or nil
// when nothing applies. It runs on the event loop with no lock held, so it must be
// fast.
type CommandCompleter interface {
	Suggest(line string) []CommandCandidate
}

// CommandCandidate is one slash-command completion: Show is the menu label,
// Apply the whole line set when it is chosen.
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

// App is the running shell. Its feeding methods are safe from any goroutine.
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
	statusFn  func(width int) string
	finalized []string
	width     int
	height    int
	resized   bool
	menu      menu
	accepted  []string
	reader    *input.Reader
	suspended bool
	// capture, when set, gets first claim on every event, ahead of the menu and
	// composer. An unconsumed event falls through.
	capture func(input.Event) bool
}

// New builds the shell and starts its frame scheduler. Call Run exactly once,
// which also releases the scheduler.
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

func newSurface(cfg Config) screen.Surface {
	if cfg.AltScreen {
		return screen.NewAltPainter(cfg.Output, cfg.Width, cfg.Height)
	}
	return screen.NewPainter(cfg.Output, cfg.Width, cfg.Height)
}

// Run consumes input events until Quit or EOF, then leaves the last frame in
// the scrollback with the cursor on a fresh line below it. It returns the
// first write error the painter hit.
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
	// Stop first so the direct paint cannot race the scheduler.
	a.sched.Stop()
	a.paint()
	a.mu.Lock()
	a.painter.Close()
	err := a.painter.Err()
	a.mu.Unlock()
	reader.Stop()
	return err
}

// Quit stops Run. It is idempotent.
func (a *App) Quit() {
	a.quitOnce.Do(func() { close(a.quit) })
}

// Append commits caller-styled lines to the scrollback above the live region on
// the next frame. They become ordinary terminal output and are never repainted.
func (a *App) Append(lines ...string) {
	if len(lines) == 0 {
		return
	}
	a.mu.Lock()
	a.finalized = append(a.finalized, lines...)
	a.mu.Unlock()
	a.sched.Request()
}

// SetLive installs the in-flight component repainted above the status line.
// Nil clears it.
func (a *App) SetLive(c screen.Component) {
	a.mu.Lock()
	a.live = c
	a.mu.Unlock()
	a.sched.Request()
}

// SetCapture installs a modal handler (an approval prompt) that gets first claim
// on every event, ahead of the menu and composer. It reports whether it consumed
// the event; the rest fall through. Nil clears it.
func (a *App) SetCapture(fn func(input.Event) bool) {
	a.mu.Lock()
	a.capture = fn
	a.mu.Unlock()
	a.sched.Request()
}

// SetStatus sets the one-row status line above the composer. Empty hides it.
func (a *App) SetStatus(line string) {
	a.mu.Lock()
	a.status = line
	a.statusFn = nil
	a.mu.Unlock()
	a.sched.Request()
}

// SetStatusFunc installs a status line built for the frame width at each paint,
// so a line that drops segments to fit is rebuilt after a resize instead of being
// cut off mid-word. fn runs with the app's lock held and must not call back into
// the app. A later [App.SetStatus] replaces it.
func (a *App) SetStatusFunc(fn func(width int) string) {
	a.mu.Lock()
	a.status = ""
	a.statusFn = fn
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

// PasteImage inserts an image chip at the cursor. The bytes travel out of line
// and surface on the next submit. Empty data is ignored.
func (a *App) PasteImage(att editor.Attachment) {
	a.mu.Lock()
	a.editor.InsertImage(att)
	a.mu.Unlock()
	a.sched.Request()
}

// cursorQuery is written while pausing the reader: the terminal's answer
// completes a blocked read so the read goroutine can park. The paused reader
// discards it, or it decodes to an ignored Unknown event after resume.
const cursorQuery = "\x1b[6n"

// Suspend hands the terminal to run: painting stops, the live region is cleared,
// and the input reader parks so run's process owns the input exclusively. After
// run returns the shell reclaims input and repaints from scratch. The caller
// leaves raw mode and the shell's terminal modes inside run and restores them
// before returning. Call it from a hook (the event loop goroutine); before Run,
// or while suspended, it does nothing.
func (a *App) Suspend(run func()) {
	a.mu.Lock()
	reader := a.reader
	if reader == nil || a.suspended {
		a.mu.Unlock()
		return
	}
	a.suspended = true
	// With suspended set the scheduler's paint is a no-op, so the poke below is
	// the only terminal writer until resume.
	a.painter.Repaint(nil)
	out := a.cfg.Output
	a.mu.Unlock()

	parked := reader.Pause(func() { _, _ = out.Write([]byte(cursorQuery)) })
	// Drain events while waiting so a full buffer cannot wedge the pump before
	// the park; keys typed after the suspending one belong to run's process.
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
	// run's process may have left the cursor mid-line.
	_, _ = out.Write([]byte("\r"))
	a.suspended = false
	a.resized = true
	a.mu.Unlock()
	a.sched.Request()
}

// Resize records a new size. The next frame repaints from scratch, since the
// terminal rewrapped the old rows and the painter's diff base is stale.
func (a *App) Resize(width, height int) {
	a.mu.Lock()
	a.width, a.height = width, height
	a.resized = true
	a.mu.Unlock()
	a.sched.Request()
}

// Width returns the current width in cells, for a host that renders its own
// content and must track resizes arriving on the event loop goroutine.
func (a *App) Width() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.width
}

// handle consumes one event on the event loop goroutine. An open menu claims
// the navigation keys first; the rest flows through the editor, then the menu
// re-derives from the composer.
func (a *App) handle(ev input.Event) {
	// Capture runs with no lock held because it reaches back into the shell.
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
		// An image-only prompt is a turn too.
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
		// None and Tab fall through to OnKey and the defaults.
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

// fallbackKey runs OnKey, then the defaults: Ctrl+C clears a non-empty prompt
// and quits on an empty one.
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

func (a *App) setContentLocked(text string) {
	a.editor.Clear()
	a.editor.Insert(text)
}

// paint runs on the scheduler's goroutine, and once on Run's after the
// scheduler stops, for the final flush.
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
		// Insert rewrites the whole frame, so it covers a resize too.
		a.painter.Insert(finalized, frame)
	case resized:
		a.painter.Repaint(frame)
	default:
		a.painter.Paint(frame)
	}
}

// composeLocked renders the live region top to bottom. The painter keeps the
// bottom rows when the frame overflows, so the composer survives clipping.
func (a *App) composeLocked() []string {
	// The blank row sets the live region apart from the transcript above.
	frame := []string{""}
	if a.live != nil {
		frame = append(frame, a.live.Render(a.width)...)
	}
	status := a.status
	if a.statusFn != nil {
		status = a.statusFn(a.width)
	}
	if status != "" {
		frame = append(frame, a.cfg.Theme.Render(theme.Status, screen.Truncate(status, a.width)))
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

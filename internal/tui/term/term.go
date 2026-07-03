// Package term manages the terminal's lifecycle around an interactive
// session: raw mode, the private modes the session needs (bracketed paste,
// focus reporting, the kitty keyboard enhancement), the terminal size, and a
// portable resize watcher. It is deliberately thin: everything stateful about
// rendering lives in the painter, everything about bytes-to-events in the
// decoder; this package only negotiates terminal state and guarantees it is
// restored, in reverse order, however the session ends.
//
// Resize is watched by polling the size through the injected clock rather
// than a signal: SIGWINCH does not exist on Windows, and the Windows console
// is a first-class target, so one mechanism serves every platform and stays
// deterministic under a manual clock in tests.
package term

import (
	"io"
	"time"

	"github.com/charmbracelet/x/ansi"
	xterm "golang.org/x/term"

	"github.com/ionalpha/flynn/clock"
)

// Options selects the terminal modes a session runs with. The zero value
// enables nothing; the session's defaults are chosen by the caller so this
// package stays policy-free.
type Options struct {
	// BracketedPaste makes a paste arrive as one delimited unit instead of a
	// burst of keystrokes.
	BracketedPaste bool
	// FocusEvents reports the terminal gaining and losing focus.
	FocusEvents bool
	// KittyKeyboard pushes the keyboard-enhancement flags that disambiguate
	// keys the legacy encoding conflates (Escape, modified Enter and Tab).
	// Terminals without the protocol ignore the push and the pop.
	KittyKeyboard bool
	// HideCursor hides the terminal's own cursor for the session. The
	// painter rests the hardware cursor at the start of the last live row,
	// not at the edit point, so the session draws a software cursor and the
	// hardware one stays out of sight until teardown restores it.
	HideCursor bool
	// AltScreen switches the session to the terminal's alternate screen buffer
	// for its lifetime, restoring the primary screen (and its scrollback) on
	// teardown. It pairs with the alternate-screen renderer for emulators where
	// inline scroll-region insertion is unsafe.
	AltScreen bool
}

// kittyFlags is the enhancement level the session uses: disambiguate escape
// codes only. Higher levels (event types, alternate keys, associated text)
// add traffic the decoder does not need yet.
const kittyFlags = 1

// Setup writes the enable sequences for every selected mode. It returns the
// first write error; a partial setup is torn down by Teardown, which
// disables unconditionally.
func Setup(w io.Writer, o Options) error {
	var seq string
	// The alternate screen goes first so every mode below, and every frame,
	// applies to the buffer the session actually draws in.
	if o.AltScreen {
		seq += ansi.SetMode(ansi.ModeAltScreenSaveCursor)
	}
	if o.BracketedPaste {
		seq += ansi.SetMode(ansi.ModeBracketedPaste)
	}
	if o.FocusEvents {
		seq += ansi.SetMode(ansi.ModeFocusEvent)
	}
	if o.KittyKeyboard {
		seq += ansi.PushKittyKeyboard(kittyFlags)
	}
	if o.HideCursor {
		seq += ansi.HideCursor
	}
	if seq == "" {
		return nil
	}
	_, err := io.WriteString(w, seq)
	return err
}

// Teardown reverses Setup in reverse order, so nested state (the kitty flag
// stack) unwinds correctly. It is safe to call after a partial or failed
// Setup: disabling a mode that was never enabled is a no-op on every
// terminal.
func Teardown(w io.Writer, o Options) error {
	var seq string
	if o.HideCursor {
		seq += ansi.ShowCursor
	}
	if o.KittyKeyboard {
		seq += ansi.PopKittyKeyboard(1)
	}
	if o.FocusEvents {
		seq += ansi.ResetMode(ansi.ModeFocusEvent)
	}
	if o.BracketedPaste {
		seq += ansi.ResetMode(ansi.ModeBracketedPaste)
	}
	// The alternate screen leaves last, restoring the primary screen and its
	// scrollback only after every mode above it has been reset.
	if o.AltScreen {
		seq += ansi.ResetMode(ansi.ModeAltScreenSaveCursor)
	}
	if seq == "" {
		return nil
	}
	_, err := io.WriteString(w, seq)
	return err
}

// MakeRaw puts the terminal identified by fd into raw mode and returns the
// restore function. The caller defers restore so a panic or an early return
// can never leave the user's shell in raw mode.
func MakeRaw(fd int) (restore func() error, err error) {
	state, err := xterm.MakeRaw(fd)
	if err != nil {
		return nil, err
	}
	return func() error { return xterm.Restore(fd, state) }, nil
}

// Size returns the terminal's current width and height in cells.
func Size(fd int) (width, height int, err error) {
	return xterm.GetSize(fd)
}

// Watcher polls the terminal size and reports changes. Stop it before
// restoring the terminal.
type Watcher struct {
	stop chan struct{}
	done chan struct{}
}

// WatchResize starts polling size (any function returning the current
// dimensions, typically a closure over Size) every interval, invoking
// onResize with each new width and height. The first call happens only on
// the first change: the caller already sized its first frame. Errors from
// size are skipped for that tick; a transiently unreadable terminal (a
// detached ConPTY during a window drag) is not a reason to tear anything
// down.
func WatchResize(timing clock.Timing, interval time.Duration, size func() (int, int, error), onResize func(w, h int)) *Watcher {
	watcher := &Watcher{stop: make(chan struct{}), done: make(chan struct{})}
	go watcher.run(timing, interval, size, onResize)
	return watcher
}

// Stop ends the watch and waits for the polling goroutine to exit, so no
// onResize can fire after Stop returns.
func (w *Watcher) Stop() {
	close(w.stop)
	<-w.done
}

func (w *Watcher) run(timing clock.Timing, interval time.Duration, size func() (int, int, error), onResize func(int, int)) {
	defer close(w.done)
	lastW, lastH, _ := size()
	for {
		timer := timing.NewTimer(interval)
		select {
		case <-w.stop:
			timer.Stop()
			return
		case <-timer.C():
		}
		curW, curH, err := size()
		if err != nil || (curW == lastW && curH == lastH) {
			continue
		}
		lastW, lastH = curW, curH
		onResize(curW, curH)
	}
}

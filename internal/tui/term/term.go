// Package term manages the terminal's lifecycle around an interactive session:
// raw mode, the private modes it needs (bracketed paste, focus reporting, the
// kitty keyboard enhancement), the terminal size and a resize watcher. It only
// negotiates terminal state and restores it in reverse order however the
// session ends; rendering and decoding live elsewhere.
//
// Resize is detected by polling through the injected clock, not a signal,
// because SIGWINCH does not exist on Windows. One mechanism serves every
// platform and is deterministic under a manual clock.
package term

import (
	"io"
	"time"

	"github.com/charmbracelet/x/ansi"
	xterm "golang.org/x/term"

	"github.com/ionalpha/flynn/clock"
)

// Options selects the terminal modes a session runs with. The zero value enables
// nothing; the caller picks the defaults.
type Options struct {
	BracketedPaste bool
	FocusEvents    bool
	// KittyKeyboard disambiguates keys the legacy encoding conflates (Escape,
	// modified Enter and Tab). Terminals without the protocol ignore it.
	KittyKeyboard bool
	// HideCursor hides the hardware cursor, which the painter rests at the start
	// of the last live row rather than the edit point; the session draws its own.
	HideCursor bool
	// AltScreen runs the session in the alternate screen, restoring the primary
	// screen and its scrollback on teardown. It pairs with the alternate-screen
	// renderer.
	AltScreen bool
}

// kittyFlags requests only disambiguated escape codes; higher levels add traffic
// the decoder does not use.
const kittyFlags = 1

// Setup writes the enable sequences for every selected mode. A partial setup is
// undone by Teardown, which disables unconditionally.
func Setup(w io.Writer, o Options) error {
	var seq string
	// First, so every mode below applies to the buffer the session draws in.
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

// Teardown reverses Setup in reverse order, so the kitty flag stack unwinds
// correctly. It is safe after a partial or failed Setup: disabling a mode never
// enabled is a no-op.
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
	// Last, so the primary screen returns only after every mode is reset.
	if o.AltScreen {
		seq += ansi.ResetMode(ansi.ModeAltScreenSaveCursor)
	}
	if seq == "" {
		return nil
	}
	_, err := io.WriteString(w, seq)
	return err
}

// MakeRaw puts fd into raw mode and returns the restore function. Defer it so a
// panic or early return cannot leave the shell in raw mode.
func MakeRaw(fd int) (restore func() error, err error) {
	state, err := xterm.MakeRaw(fd)
	if err != nil {
		return nil, err
	}
	return func() error { return xterm.Restore(fd, state) }, nil
}

// Size returns the width and height of fd in cells.
func Size(fd int) (width, height int, err error) {
	return xterm.GetSize(fd)
}

// Watcher polls the terminal size. Stop it before restoring the terminal.
type Watcher struct {
	stop chan struct{}
	done chan struct{}
}

// WatchResize polls size every interval and hands each reading to onResize, which
// ignores one equal to the size it has. The watcher keeps no copy to compare with:
// the caller also reads the size between ticks, so a window dragged away and back
// within one interval would look unchanged here while the caller held the size in
// between. A size error skips that tick, since a terminal can be briefly
// unreadable (a detached ConPTY during a window drag).
func WatchResize(timing clock.Timing, interval time.Duration, size func() (int, int, error), onResize func(w, h int)) *Watcher {
	watcher := &Watcher{stop: make(chan struct{}), done: make(chan struct{})}
	go watcher.run(timing, interval, size, onResize)
	return watcher
}

// Stop waits for the polling goroutine, so no onResize fires after it returns.
func (w *Watcher) Stop() {
	close(w.stop)
	<-w.done
}

func (w *Watcher) run(timing clock.Timing, interval time.Duration, size func() (int, int, error), onResize func(int, int)) {
	defer close(w.done)
	for {
		timer := timing.NewTimer(interval)
		select {
		case <-w.stop:
			timer.Stop()
			return
		case <-timer.C():
		}
		if curW, curH, err := size(); err == nil {
			onResize(curW, curH)
		}
	}
}

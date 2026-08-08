package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/internal/tui/app"
	"github.com/ionalpha/flynn/internal/tui/editor"
	tuiterm "github.com/ionalpha/flynn/internal/tui/term"
	"github.com/ionalpha/flynn/internal/tui/theme"
	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/session"
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
func runInteractiveTUI(ctx context.Context, s *replSession) error {
	fd := int(os.Stdin.Fd())
	restore, err := tuiterm.MakeRaw(fd)
	if err != nil {
		// No raw mode means no shell; the line interface still works.
		return s.runLineMode(ctx, s.cwd)
	}
	alt := preferAltScreen(os.Getenv)
	modes := tuiterm.Options{BracketedPaste: true, KittyKeyboard: true, HideCursor: true, AltScreen: alt}
	if err := tuiterm.Setup(os.Stdout, modes); err != nil {
		_ = restore()
		return err
	}

	width, height, _ := tuiterm.Size(fd) // zero on error selects the shell's defaults
	a, host := newSessionShell(ctx, s, os.Stdin, os.Stdout, width, height, alt)
	// The editor handoff owns the terminal's mode round trip: cooked mode and
	// shell modes off while the user's editor runs, raw mode and shell modes
	// back before the shell repaints. It reassigns restore so the session's
	// closing restore releases the latest raw state, not a stale one.
	host.edit = func(initial string) (string, error) {
		return editExternal(initial, func() (func() error, error) {
			if err := tuiterm.Teardown(os.Stdout, modes); err != nil {
				return nil, err
			}
			if err := restore(); err != nil {
				return nil, err
			}
			return func() error {
				again, err := tuiterm.MakeRaw(fd)
				if err != nil {
					return err
				}
				restore = again
				return tuiterm.Setup(os.Stdout, modes)
			}, nil
		})
	}
	watcher := tuiterm.WatchResize(clock.System{}, resizePoll, func() (int, int, error) {
		return tuiterm.Size(fd)
	}, a.Resize)

	host.greet()
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
func newSessionShell(ctx context.Context, s *replSession, in io.Reader, out io.Writer, width, height int, altScreen bool) (*app.App, *sessionHost) {
	th := s.theme
	if th == nil {
		th = theme.Default()
	}
	host := &sessionHost{
		ctx:      ctx,
		s:        s,
		th:       th,
		clip:     tuiterm.NewClipboard(),
		tv:       newTranscriptView(th),
		live:     &activity{th: th},
		panel:    &govPanel{th: th},
		approval: &approvalPrompt{th: th},
		timing:   clock.System{},
		proj:     session.NewProjection(),
	}
	// Shell mode runs the user's commands through the same confined sandbox
	// every other command takes, rooted at the session's working directory. A
	// directory the sandbox refuses is reported when shell mode is first used;
	// the rest of the session works without it.
	if run, err := sandbox.NewLocal(s.cwd); err != nil {
		host.runErr = err
	} else {
		host.run = run
	}
	a := app.New(app.Config{
		Input:       in,
		Output:      out,
		Width:       width,
		Height:      height,
		Theme:       host.th,
		Placeholder: "Send a message",
		Keymap:      s.keys,
		OnSubmit:    host.submit,
		OnEsc:       host.onEsc,
		OnKey:       host.key,
		Completer:   newFileCompleter(s.cwd),
		Commands:    newCommandCompleter(),
		Marker:      shellMarker,
		AltScreen:   altScreen,
	})
	host.ui = a
	// Recall (and any future out-of-band note) lands in the scrollback as a muted
	// line, so what the agent pulled in from earlier runs is visible.
	s.notice = func(text string) { a.Append(th.Render(theme.Muted, "  "+text)) }
	// This is the shell that can actually ask a person, so this is where the prompter is
	// installed. A paused action raises the modal overlay and the operator's allow or
	// deny resolves it; without this the session would have a gate and nobody to ask, and
	// every listed action would be refused.
	s.gates.prompter = approvalPrompter{host: host}
	return a, host
}

// preferAltScreen decides whether the session runs on the alternate screen.
// The inline renderer is the default because it keeps the transcript in the
// terminal's own scrollback; the alternate screen is the fallback for
// multiplexers where inline scroll-region insertion is unsafe. FLYNN_TUI_ALTSCREEN
// forces the choice either way ("1"/"true"/"on" or "0"/"false"/"off"); with no
// override it turns on inside Zellij, which sets ZELLIJ in the environment.
func preferAltScreen(getenv func(string) string) bool {
	switch strings.ToLower(strings.TrimSpace(getenv("FLYNN_TUI_ALTSCREEN"))) {
	case "1", "true", "on", "yes":
		return true
	case "0", "false", "off", "no":
		return false
	}
	return getenv("ZELLIJ") != ""
}

// loadKeymap reads the user's composer bindings from keymap.json in the data
// directory, layered over the default map. No file means the defaults; a file
// that fails to parse is an error, reported before the session starts, since
// silently discarding a user's bindings would be undebuggable from inside the
// shell.
func loadKeymap(dataDir string) (editor.Keymap, error) {
	f, err := os.Open(filepath.Join(dataDir, "keymap.json")) //nolint:gosec // G304: fixed filename under the operator-chosen data dir
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	km, err := editor.LoadKeymap(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", f.Name(), err)
	}
	return km, nil
}

// loadTheme reads the user's theme from theme.json in the data directory,
// layered over its declared built-in base. No file means the default theme; a
// file that fails to parse is an error, reported before the session starts,
// since silently falling back to the default would leave a user's theme
// quietly ignored with nothing to point at.
func loadTheme(dataDir string) (*theme.Theme, error) {
	f, err := os.Open(filepath.Join(dataDir, "theme.json")) //nolint:gosec // G304: fixed filename under the operator-chosen data dir
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	th, err := theme.Load(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", f.Name(), err)
	}
	return th, nil
}

// shellMarker swaps the composer's prompt marker for the shell marker while
// the prompt is a shell command, so the input mode is visible as it is typed.
func shellMarker(content string) string {
	if strings.HasPrefix(content, "!") {
		return "! "
	}
	return ""
}

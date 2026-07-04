package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/internal/tui/app"
	"github.com/ionalpha/flynn/internal/tui/editor"
	"github.com/ionalpha/flynn/internal/tui/input"
	"github.com/ionalpha/flynn/internal/tui/screen"
	tuiterm "github.com/ionalpha/flynn/internal/tui/term"
	"github.com/ionalpha/flynn/internal/tui/theme"
	"github.com/ionalpha/flynn/llm"
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
func runInteractiveTUI(ctx context.Context, s *replSession, seed string) error {
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
func newSessionShell(ctx context.Context, s *replSession, in io.Reader, out io.Writer, width, height int, altScreen bool) (*app.App, *sessionHost) {
	th := s.theme
	if th == nil {
		th = theme.Default()
	}
	host := &sessionHost{
		ctx:   ctx,
		s:     s,
		th:    th,
		clip:  tuiterm.NewClipboard(),
		tv:    newTranscriptView(th),
		live:  &activity{th: th},
		panel: &govPanel{th: th},
		proj:  session.NewProjection(),
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
		Marker:      shellMarker,
		AltScreen:   altScreen,
	})
	host.ui = a
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

// shellUI is the slice of the shell the session host drives. An interface so
// tests can observe the host through a recording fake.
type shellUI interface {
	Append(lines ...string)
	SetLive(c screen.Component)
	SetStatus(line string)
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
func (h *sessionHost) greet(seed string) {
	h.liveComp = liveStack{h.panel, h.live}
	h.panel.set(h.proj)
	h.ui.SetLive(h.liveComp)
	h.ui.Append(h.th.Render(theme.Status, "flynn interactive session in "+h.s.cwd))
	if seed != "" {
		h.ui.Append(strings.Split(strings.TrimRight(seed, "\n"), "\n")...)
	}
	h.refreshStatus()
}

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
	switch t.text {
	case "/seal":
		h.startRecord(h.doSeal)
		return
	case "/verify":
		h.startRecord(h.doVerify)
		return
	case "/fork":
		h.startRecord(h.doFork)
		return
	case "/replay":
		h.startRecord(h.doReplay)
		return
	}
	turnCtx, cancel := context.WithCancel(h.ctx)
	h.mu.Lock()
	h.cancel = cancel
	h.mu.Unlock()
	h.refreshStatus()

	h.ui.Append("", h.th.Render(theme.UserPrefix, "> ")+h.th.Render(theme.UserText, promptEcho(t)))
	// The turn driver taps every session event through the observer: the shell
	// renders the typed stream itself (transcript, governance, badge), so the
	// driver's flat text goes nowhere.
	h.s.out = io.Discard
	h.s.observer = h.onEvent
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

	h.ui.Append("", h.th.Render(theme.UserPrefix, "! ")+h.th.Render(theme.UserText, cmdLine))
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

// doSeal seals the session's run into a verifiable record and moves the status badge to
// sealed. The seal appends the record to the run's own stream; because the shell does
// not subscribe to the stream at idle, it folds the sealed state into the projection
// directly, the same state a live record.sealed event would produce, so the badge
// tracks it. A failure is reported inline and leaves the badge unchanged.
func (h *sessionHost) doSeal(ctx context.Context) {
	h.ui.Append("", h.th.Render(theme.UserPrefix, "> ")+h.th.Render(theme.UserText, "/seal"))
	if err := h.s.seal(ctx); err != nil {
		h.ui.Append(h.th.Render(theme.Rejected, "  "+err.Error()))
		return
	}
	h.foldRecord(session.Event{Kind: session.KindRecordSealed})
	h.ui.Append(h.th.Render(theme.Success, "  run sealed; /verify to check it"))
}

// doVerify verifies the session's sealed record, prints its per-tier report to the
// scrollback, and moves the badge to verified when every tier passes. A run not yet
// sealed, or a tier that fails, is reported and leaves the badge unchanged.
func (h *sessionHost) doVerify(ctx context.Context) {
	h.ui.Append("", h.th.Render(theme.UserPrefix, "> ")+h.th.Render(theme.UserText, "/verify"))
	var buf bytes.Buffer
	err := h.s.verify(ctx, &buf)
	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		h.ui.Append(h.th.Render(theme.ToolOutput, "  "+line))
	}
	if err != nil {
		// A failed tier is already named in the report above; only a plain error (an
		// unsealed run) needs a line of its own.
		if !errors.Is(err, errChecksFailed) {
			h.ui.Append(h.th.Render(theme.Rejected, "  "+err.Error()))
		}
		return
	}
	h.foldRecord(session.Event{Kind: session.KindRecordVerified})
	h.ui.Append(h.th.Render(theme.Success, "  record verified"))
}

// doFork branches the run into a new independent run seeded with the conversation so far
// and switches the session onto it, resetting the record badge to the fork's own fresh
// recording state. The original run keeps its history and seal. The next prompt continues
// on the fork; a failure is reported inline and leaves the session on the original run.
func (h *sessionHost) doFork(ctx context.Context) {
	h.ui.Append("", h.th.Render(theme.UserPrefix, "> ")+h.th.Render(theme.UserText, "/fork"))
	forkID, err := h.s.fork(ctx)
	if err != nil {
		h.ui.Append(h.th.Render(theme.Rejected, "  "+err.Error()))
		return
	}
	// The fork's stream is empty, so its badge starts at the fresh recording state; the
	// next turn's events repopulate the governance projection from the branch point.
	h.mu.Lock()
	h.proj = session.NewProjection()
	p := h.proj
	h.mu.Unlock()
	h.panel.set(p)
	h.refreshStatus()
	h.ui.Append(h.th.Render(theme.Success, "  forked to run "+forkID+"; the original is untouched"))
}

// doReplay re-renders the run's recorded events into the scrollback through the themed
// transcript renderer, between clear delimiters. It reads the run's history from the
// durable store and folds it through a fresh transcript view, so the replay is the run
// as it was recorded (the same markdown and governance rendering a live turn produces),
// independent of what is currently on screen. It is a pure read; it changes no run state.
func (h *sessionHost) doReplay(ctx context.Context) {
	h.ui.Append("", h.th.Render(theme.UserPrefix, "> ")+h.th.Render(theme.UserText, "/replay"))
	events, err := session.History(ctx, h.s.store.Log(), h.s.runID)
	if err != nil {
		h.ui.Append(h.th.Render(theme.Rejected, "  replay failed: "+err.Error()))
		return
	}
	if len(events) == 0 {
		h.ui.Append(h.th.Render(theme.Status, "  nothing recorded to replay yet"))
		return
	}
	h.ui.Append(h.th.Render(theme.Status, fmt.Sprintf("  replay of run %s (%d events)", h.s.runID, len(events))))
	tv := newTranscriptView(h.th)
	width := h.ui.Width()
	for _, ev := range events {
		if lines := tv.lines(ev, width); len(lines) > 0 {
			h.ui.Append(lines...)
		}
	}
	h.ui.Append(h.th.Render(theme.Status, "  end of replay"))
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
	}
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
		return "enter sends · alt+enter or ctrl+j newline · @ mentions a file · ! runs a shell command · /seal + /verify record the run · /replay re-renders it · /fork branches it · ctrl+o governance · ctrl+g opens $EDITOR · ctrl+v pastes an image · up/down history · ctrl+d quits"
	}
	line := "working... esc or ctrl+c cancels"
	if queued > 0 {
		line += fmt.Sprintf(" · %d queued", queued)
	}
	return line
}

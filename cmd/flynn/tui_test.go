package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ionalpha/flynn/internal/tui/editor"
	"github.com/ionalpha/flynn/internal/tui/input"
	"github.com/ionalpha/flynn/internal/tui/screen"
	tuiterm "github.com/ionalpha/flynn/internal/tui/term"
	"github.com/ionalpha/flynn/internal/tui/theme"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/llm/llmtest"
	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/session"
)

func TestPreferAltScreen(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want bool
	}{
		{"default off", nil, false},
		{"zellij on", map[string]string{"ZELLIJ": "0"}, true},
		{"override on", map[string]string{"FLYNN_TUI_ALTSCREEN": "1"}, true},
		{"override yes", map[string]string{"FLYNN_TUI_ALTSCREEN": " YES "}, true},
		{"override off beats zellij", map[string]string{"FLYNN_TUI_ALTSCREEN": "off", "ZELLIJ": "0"}, false},
		{"override false", map[string]string{"FLYNN_TUI_ALTSCREEN": "false"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			getenv := func(k string) string { return c.env[k] }
			if got := preferAltScreen(getenv); got != c.want {
				t.Fatalf("preferAltScreen(%v) = %v, want %v", c.env, got, c.want)
			}
		})
	}
}

// fakeUI records what the session host drives, standing in for a live shell.
type fakeUI struct {
	mu       sync.Mutex
	lines    []string
	status   string
	quit     bool
	draft    string
	suspends int
	pasted   []editor.Attachment
	capture  func(input.Event) bool
	live     screen.Component
}

func (f *fakeUI) Append(lines ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lines = append(f.lines, lines...)
}

func (f *fakeUI) SetLive(c screen.Component) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.live = c
}

// SetCapture records the modal input handler the host installs, so a test can
// feed events through it exactly as the app's event loop would.
func (f *fakeUI) SetCapture(fn func(input.Event) bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.capture = fn
}

// captureFn returns the currently installed modal handler, or nil.
func (f *fakeUI) captureFn() func(input.Event) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.capture
}

// liveLines renders the installed live region at the given width, or nil when
// none is installed, so a test can observe what the overlay paints.
func (f *fakeUI) liveLines(width int) []string {
	f.mu.Lock()
	c := f.live
	f.mu.Unlock()
	if c == nil {
		return nil
	}
	return c.Render(width)
}

func (f *fakeUI) SetStatus(line string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = line
}

func (f *fakeUI) Quit() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.quit = true
}

// Suspend counts the handoff and runs the callback inline; the fake has no
// terminal or reader to park.
func (f *fakeUI) Suspend(run func()) {
	f.mu.Lock()
	f.suspends++
	f.mu.Unlock()
	run()
}

func (f *fakeUI) Draft() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.draft
}

func (f *fakeUI) SetDraft(text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.draft = text
}

func (f *fakeUI) PasteImage(att editor.Attachment) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pasted = append(f.pasted, att)
}

// Width reports a fixed terminal width, enough for the transcript renderer to
// wrap against without a live terminal.
func (f *fakeUI) Width() int { return 80 }

func (f *fakeUI) pastedImages() []editor.Attachment {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]editor.Attachment(nil), f.pasted...)
}

// fakeClip is a clipboard whose image content the test sets directly, standing
// in for the OS clipboard.
type fakeClip struct {
	data []byte
}

func (c fakeClip) Image() ([]byte, bool) {
	if len(c.data) == 0 {
		return nil, false
	}
	return c.data, true
}

func (f *fakeUI) transcript() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.lines, "\n")
}

func (f *fakeUI) quitCalled() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.quit
}

// newHostForTest builds a session host over an in-memory session and the
// given model, driving a recording fake instead of a terminal.
func newHostForTest(t *testing.T, model llm.Model) (*sessionHost, *fakeUI) {
	t.Helper()
	s, _ := newREPL(t, t.TempDir(), memStore(t), model)
	ui := &fakeUI{}
	th := theme.Default()
	host := &sessionHost{
		ctx:      context.Background(),
		s:        s,
		ui:       ui,
		th:       th,
		tv:       newTranscriptView(th),
		live:     &activity{th: th},
		panel:    &govPanel{th: th},
		approval: &approvalPrompt{th: th},
		proj:     session.NewProjection(),
	}
	host.liveComp = liveStack{host.approval, host.panel, host.live}
	// Mirror newSessionShell's out-of-band note wiring so tests exercise the real path.
	s.notice = func(text string) { ui.Append(th.Render(theme.Muted, "  "+text)) }
	return host, ui
}

// waitIdle blocks until the host has no running turn and an empty queue,
// failing the test if it never settles.
func waitIdle(t *testing.T, h *sessionHost) {
	t.Helper()
	deadline := time.After(15 * time.Second)
	for {
		h.mu.Lock()
		idle := !h.busy && len(h.queue) == 0
		h.mu.Unlock()
		if idle {
			return
		}
		select {
		case <-deadline:
			t.Fatal("session host never returned to idle")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// TestShellHostRunsTurnAndAppendsTranscript proves a submitted prompt drives
// a real turn through the same driver the line interface uses: the prompt
// echo and the model's answer both land in the scrollback, and the host
// returns to idle.
func TestShellHostRunsTurnAndAppendsTranscript(t *testing.T) {
	host, ui := newHostForTest(t, llmtest.NewScripted(llmtest.SayText("first answer")))
	host.submit("hello there", nil)
	waitIdle(t, host)

	got := ui.transcript()
	for _, want := range []string{"hello there", "first answer"} {
		if !strings.Contains(got, want) {
			t.Fatalf("transcript missing %q:\n%s", want, got)
		}
	}
}

// TestShellHostCancelKeepsSession proves interrupting an in-flight turn
// cancels only that turn: the transcript notes the cancellation and the host
// returns to idle with the session intact.
func TestShellHostCancelKeepsSession(t *testing.T) {
	gm := &gateModel{entered: make(chan struct{}), release: make(chan struct{})}
	host, ui := newHostForTest(t, gm)
	host.submit("long running", nil)

	select {
	case <-gm.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("turn never reached the model")
	}
	host.interrupt()
	waitIdle(t, host)

	if got := ui.transcript(); !strings.Contains(got, "cancelled") {
		t.Fatalf("transcript did not note the cancellation:\n%s", got)
	}
	if host.s.runID == "" {
		t.Fatal("a cancelled first turn still opens the run, but no id was recorded")
	}
}

// TestShellHostQueuesPromptWhileBusy proves a prompt submitted during a
// running turn queues behind it and runs when the turn ends, in order.
func TestShellHostQueuesPromptWhileBusy(t *testing.T) {
	gm := &gateModel{entered: make(chan struct{}), release: make(chan struct{})}
	host, ui := newHostForTest(t, gm)
	host.submit("one", nil)

	select {
	case <-gm.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("turn never reached the model")
	}
	host.submit("two", nil)
	host.mu.Lock()
	queued := len(host.queue)
	host.mu.Unlock()
	if queued != 1 {
		t.Fatalf("queue length = %d, want 1", queued)
	}
	if !strings.Contains(host.statusNow(), "queued") {
		t.Fatal("status line does not show the queued prompt")
	}

	close(gm.release)
	waitIdle(t, host)

	got := ui.transcript()
	first, second := strings.Index(got, "one"), strings.Index(got, "two")
	if first < 0 || second < 0 {
		t.Fatalf("transcript missing an echoed prompt:\n%s", got)
	}
	if second < first {
		t.Fatalf("queued prompt ran out of order:\n%s", got)
	}
}

// fakeRunner is a scripted shellRunner recording the commands shell mode
// runs. When block is set, Exec waits for the channel or the context, so a
// test can hold a command in flight.
type fakeRunner struct {
	res   sandbox.ExecResult
	err   error
	block chan struct{}

	mu   sync.Mutex
	cmds []string
}

func (f *fakeRunner) Exec(ctx context.Context, cmd sandbox.Command) (sandbox.ExecResult, error) {
	f.mu.Lock()
	f.cmds = append(f.cmds, cmd.Line)
	f.mu.Unlock()
	if f.block != nil {
		select {
		case <-ctx.Done():
			return sandbox.ExecResult{}, ctx.Err()
		case <-f.block:
		}
	}
	return f.res, f.err
}

// TestShellHostBangRunsConfinedCommand proves a "!" prompt runs through the
// host's runner instead of a model turn: the command is echoed, its output
// lands in the scrollback, and the host returns to idle.
func TestShellHostBangRunsConfinedCommand(t *testing.T) {
	host, ui := newHostForTest(t, llmtest.NewScripted())
	run := &fakeRunner{res: sandbox.ExecResult{Output: "alpha\nbeta\n"}}
	host.run = run
	host.submit("!echo hi", nil)
	waitIdle(t, host)

	got := ui.transcript()
	for _, want := range []string{"! ", "echo hi", "alpha", "beta"} {
		if !strings.Contains(got, want) {
			t.Fatalf("transcript missing %q:\n%s", want, got)
		}
	}
	run.mu.Lock()
	cmds := run.cmds
	run.mu.Unlock()
	if len(cmds) != 1 || cmds[0] != "echo hi" {
		t.Fatalf("runner saw %v, want the bare command line", cmds)
	}
	if host.s.started {
		t.Fatal("a shell command opened a model run")
	}
}

// TestShellHostBangReportsExitCode proves a failing command's exit code is
// committed to the transcript; a zero exit stays silent.
func TestShellHostBangReportsExitCode(t *testing.T) {
	host, ui := newHostForTest(t, llmtest.NewScripted())
	host.run = &fakeRunner{res: sandbox.ExecResult{ExitCode: 2}}
	host.submit("!false", nil)
	waitIdle(t, host)
	if got := ui.transcript(); !strings.Contains(got, "exit 2") {
		t.Fatalf("transcript missing the exit code:\n%s", got)
	}

	host, ui = newHostForTest(t, llmtest.NewScripted())
	host.run = &fakeRunner{res: sandbox.ExecResult{Output: "ok\n"}}
	host.submit("!true", nil)
	waitIdle(t, host)
	if got := ui.transcript(); strings.Contains(got, "exit") {
		t.Fatalf("zero exit was reported:\n%s", got)
	}
}

// TestShellHostBangCancel proves Escape cancels an in-flight shell command
// the same way it cancels a turn: the transcript notes it and the session
// stays live.
func TestShellHostBangCancel(t *testing.T) {
	host, ui := newHostForTest(t, llmtest.NewScripted())
	host.run = &fakeRunner{block: make(chan struct{})}
	host.submit("!sleep forever", nil)
	host.interrupt()
	waitIdle(t, host)
	if got := ui.transcript(); !strings.Contains(got, "command cancelled") {
		t.Fatalf("transcript did not note the cancellation:\n%s", got)
	}
}

// TestShellHostBangEdgeCases covers the bare "!" usage hint and a session
// whose sandbox could not be built.
func TestShellHostBangEdgeCases(t *testing.T) {
	host, ui := newHostForTest(t, llmtest.NewScripted())
	host.run = &fakeRunner{}
	host.submit("!", nil)
	waitIdle(t, host)
	if got := ui.transcript(); !strings.Contains(got, "usage:") {
		t.Fatalf("bare ! did not show the usage hint:\n%s", got)
	}

	host, ui = newHostForTest(t, llmtest.NewScripted())
	host.submit("!echo hi", nil)
	waitIdle(t, host)
	if got := ui.transcript(); !strings.Contains(got, "shell mode unavailable") {
		t.Fatalf("missing runner was not reported:\n%s", got)
	}
}

// TestShellHostBangQueuesBehindTurn proves a shell command submitted during
// a running turn waits its turn and runs after it, in submission order.
func TestShellHostBangQueuesBehindTurn(t *testing.T) {
	gm := &gateModel{entered: make(chan struct{}), release: make(chan struct{})}
	host, ui := newHostForTest(t, gm)
	host.run = &fakeRunner{res: sandbox.ExecResult{Output: "later\n"}}
	host.submit("one", nil)
	select {
	case <-gm.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("turn never reached the model")
	}
	host.submit("!echo later", nil)
	close(gm.release)
	waitIdle(t, host)

	got := ui.transcript()
	turn, cmd := strings.Index(got, "one"), strings.Index(got, "echo later")
	if turn < 0 || cmd < 0 || cmd < turn {
		t.Fatalf("queued shell command ran out of order:\n%s", got)
	}
}

// TestShellMarker pins the composer marker hook: a "!" prompt carries the
// shell marker, anything else keeps the default.
func TestShellMarker(t *testing.T) {
	if got := shellMarker("!ls"); got != "! " {
		t.Fatalf("shellMarker(!ls) = %q", got)
	}
	if got := shellMarker("hello"); got != "" {
		t.Fatalf("shellMarker(hello) = %q", got)
	}
}

// statusNow reads the current status line through the host's own fake.
func (h *sessionHost) statusNow() string {
	f, ok := h.ui.(*fakeUI)
	if !ok {
		return ""
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status
}

// TestShellHostExitQuitsWithoutTurn proves an exit command closes the shell
// and never opens a run.
func TestShellHostExitQuitsWithoutTurn(t *testing.T) {
	host, ui := newHostForTest(t, llmtest.NewScripted())
	host.submit("exit", nil)
	if !ui.quitCalled() {
		t.Fatal("exit command did not quit the shell")
	}
	if host.s.started {
		t.Fatal("exit command opened a run")
	}
}

// TestShellHostKeys covers the session-level key routing: Ctrl+C cancels
// only while a turn runs, Ctrl+D quits only at idle, and both stay unclaimed
// otherwise so the shell's defaults apply.
func TestShellHostKeys(t *testing.T) {
	ctrl := func(c rune) input.Key { return input.Key{Code: c, Mods: input.ModCtrl} }

	host, ui := newHostForTest(t, llmtest.NewScripted())
	if host.key(ctrl('c')) {
		t.Fatal("idle Ctrl+C was claimed; it belongs to the shell's defaults")
	}
	if !host.key(ctrl('d')) || !ui.quitCalled() {
		t.Fatal("idle Ctrl+D did not quit")
	}

	gm := &gateModel{entered: make(chan struct{}), release: make(chan struct{})}
	host, _ = newHostForTest(t, gm)
	host.submit("long running", nil)
	select {
	case <-gm.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("turn never reached the model")
	}
	if host.key(ctrl('d')) {
		t.Fatal("busy Ctrl+D was claimed; quitting mid-turn is the composer's Delete")
	}
	if !host.key(ctrl('c')) {
		t.Fatal("busy Ctrl+C did not cancel the turn")
	}
	waitIdle(t, host)
}

// TestShellHostPasteImage proves Ctrl+V lands a clipboard image in the
// composer through the clipboard port, that an empty clipboard degrades to a
// one-line notice, and that a host with no clipboard leaves Ctrl+V unclaimed.
func TestShellHostPasteImage(t *testing.T) {
	ctrlV := input.Key{Code: 'v', Mods: input.ModCtrl}

	// An image on the clipboard becomes a composer chip.
	host, ui := newHostForTest(t, llmtest.NewScripted())
	host.clip = fakeClip{data: []byte("PNGDATA")}
	if !host.key(ctrlV) {
		t.Fatal("Ctrl+V with a clipboard was not claimed")
	}
	pasted := ui.pastedImages()
	if len(pasted) != 1 || string(pasted[0].Data) != "PNGDATA" || pasted[0].MediaType != tuiterm.ImagePNG {
		t.Fatalf("pasted = %+v, want one image/png PNGDATA", pasted)
	}

	// An empty clipboard says so rather than pasting nothing silently.
	host, ui = newHostForTest(t, llmtest.NewScripted())
	host.clip = fakeClip{}
	if !host.key(ctrlV) {
		t.Fatal("Ctrl+V with an empty clipboard was not claimed")
	}
	if len(ui.pastedImages()) != 0 {
		t.Fatal("an empty clipboard must not paste an image")
	}
	if !strings.Contains(ui.transcript(), "no image on the clipboard") {
		t.Fatalf("empty clipboard notice missing from %q", ui.transcript())
	}

	// No clipboard at all leaves the key for the shell's defaults.
	host, _ = newHostForTest(t, llmtest.NewScripted())
	if host.key(ctrlV) {
		t.Fatal("Ctrl+V was claimed with no clipboard configured")
	}
}

// TestShellHostPasteCommand proves the /paste fallback lands a clipboard image
// in the composer instead of running as a prompt, for terminals that swallow
// Ctrl+V.
func TestShellHostPasteCommand(t *testing.T) {
	host, ui := newHostForTest(t, llmtest.NewScripted())
	host.clip = fakeClip{data: []byte("FROMCMD")}
	host.submit("/paste", nil)
	waitIdle(t, host)

	pasted := ui.pastedImages()
	if len(pasted) != 1 || string(pasted[0].Data) != "FROMCMD" {
		t.Fatalf("/paste pasted = %+v, want the clipboard image", pasted)
	}
	// It must not have run as a turn: no prompt echo for "/paste".
	if strings.Contains(ui.transcript(), "/paste") {
		t.Fatalf("/paste ran as a prompt: %q", ui.transcript())
	}
}

// TestShellHostImageOnlySubmitReachesModel proves an image-only submit (a
// pasted picture, no prose) drives a real turn and hands the model the image.
func TestShellHostImageOnlySubmitReachesModel(t *testing.T) {
	model := llmtest.NewScripted(llmtest.SayText("done"))
	host, ui := newHostForTest(t, model)
	host.submit("", []editor.Attachment{{MediaType: tuiterm.ImagePNG, Data: []byte("shot")}})
	waitIdle(t, host)

	reqs := model.Requests()
	if len(reqs) == 0 {
		t.Fatal("image-only submit drove no model turn")
	}
	var sawImage bool
	for _, b := range reqs[0].Messages[0].Blocks {
		if b.Kind == llm.KindImage && b.Image != nil && string(b.Image.Data) == "shot" {
			sawImage = true
		}
	}
	if !sawImage {
		t.Fatalf("model's opening turn did not carry the image: %+v", reqs[0].Messages[0].Blocks)
	}
	// The transcript echoes the image count, not a blank prompt line.
	if !strings.Contains(ui.transcript(), "[1 image]") {
		t.Fatalf("image-only echo missing from %q", ui.transcript())
	}
}

// syncOut is a concurrency-safe frame sink for the end-to-end shell test.
type syncOut struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncOut) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncOut) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestSessionShellEndToEnd drives the production wiring over pipes: typed
// keystrokes reach the composer, Enter submits and runs a real turn, the
// answer is painted, and Ctrl+D on the empty composer ends the shell.
func TestSessionShellEndToEnd(t *testing.T) {
	s, _ := newREPL(t, t.TempDir(), memStore(t), llmtest.NewScripted(llmtest.SayText("first answer")))
	pr, pw := io.Pipe()
	out := &syncOut{}
	a, host := newSessionShell(context.Background(), s, pr, out, 80, 24, false)
	host.greet()

	done := make(chan error, 1)
	go func() { done <- a.Run() }()

	if _, err := pw.Write([]byte("hello there\r")); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(15 * time.Second)
	for !strings.Contains(out.String(), "first answer") {
		select {
		case <-deadline:
			t.Fatalf("answer never painted:\n%s", out.String())
		case <-time.After(10 * time.Millisecond):
		}
	}
	waitIdle(t, host)

	if _, err := pw.Write([]byte{0x04}); err != nil { // Ctrl+D on the empty composer
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shell exited with error: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Ctrl+D did not end the shell")
	}
	host.shutdown()
	_ = pw.Close()
}

// TestSessionShellAltScreenEndToEnd drives the same production wiring with the
// alternate-screen renderer selected: the turn still runs and its answer still
// paints, and the output carries the absolute row addressing only the alt-screen
// painter emits, confirming the AltScreen config reaches the renderer.
func TestSessionShellAltScreenEndToEnd(t *testing.T) {
	s, _ := newREPL(t, t.TempDir(), memStore(t), llmtest.NewScripted(llmtest.SayText("alt answer")))
	pr, pw := io.Pipe()
	out := &syncOut{}
	a, host := newSessionShell(context.Background(), s, pr, out, 80, 24, true)
	host.greet()

	done := make(chan error, 1)
	go func() { done <- a.Run() }()

	if _, err := pw.Write([]byte("hello there\r")); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(15 * time.Second)
	for !strings.Contains(out.String(), "alt answer") {
		select {
		case <-deadline:
			t.Fatalf("answer never painted:\n%s", out.String())
		case <-time.After(10 * time.Millisecond):
		}
	}
	// The alt-screen painter addresses rows absolutely (CSI row;1H); the inline
	// painter never does, so this is proof the alternate renderer is in play.
	if !strings.Contains(out.String(), ";1H") {
		t.Fatalf("no absolute row addressing in alt-screen output:\n%q", out.String())
	}
	waitIdle(t, host)

	if _, err := pw.Write([]byte{0x04}); err != nil { // Ctrl+D on the empty composer
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shell exited with error: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Ctrl+D did not end the shell")
	}
	host.shutdown()
	_ = pw.Close()
}

// TestSessionShellFileCompletion checks the production wiring serves the
// session's working directory through the composer's @-menu: typing @ plus
// a query paints matching files, and Tab splices the pick into the prompt.
func TestSessionShellFileCompletion(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "alpha_notes.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, _ := newREPL(t, dir, memStore(t), llmtest.NewScripted(llmtest.SayText("ok")))
	pr, pw := io.Pipe()
	out := &syncOut{}
	a, host := newSessionShell(context.Background(), s, pr, out, 80, 24, false)
	host.greet()

	done := make(chan error, 1)
	go func() { done <- a.Run() }()

	awaitPaint := func(want string) {
		t.Helper()
		deadline := time.After(15 * time.Second)
		for !strings.Contains(out.String(), want) {
			select {
			case <-deadline:
				t.Fatalf("%q never painted:\n%s", want, out.String())
			case <-time.After(10 * time.Millisecond):
			}
		}
	}

	if _, err := pw.Write([]byte("@alpha")); err != nil {
		t.Fatal(err)
	}
	awaitPaint("> alpha_notes.md")
	if _, err := pw.Write([]byte("\t")); err != nil { // Tab accepts the pick
		t.Fatal(err)
	}
	awaitPaint("@alpha_notes.md ")

	a.Quit()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("shell did not stop")
	}
	host.shutdown()
	_ = pw.Close()
}

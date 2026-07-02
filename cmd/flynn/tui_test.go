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

	"github.com/ionalpha/flynn/internal/tui/input"
	"github.com/ionalpha/flynn/internal/tui/screen"
	"github.com/ionalpha/flynn/internal/tui/theme"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/llm/llmtest"
)

// fakeUI records what the session host drives, standing in for a live shell.
type fakeUI struct {
	mu     sync.Mutex
	lines  []string
	status string
	quit   bool
}

func (f *fakeUI) Append(lines ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lines = append(f.lines, lines...)
}

func (f *fakeUI) SetLive(screen.Component) {}

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
	return &sessionHost{ctx: context.Background(), s: s, ui: ui, th: theme.Default()}, ui
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
	host.submit("hello there")
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
	host.submit("long running")

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
	host.submit("one")

	select {
	case <-gm.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("turn never reached the model")
	}
	host.submit("two")
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
	host.submit("exit")
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
	host.submit("long running")
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
	a, host := newSessionShell(context.Background(), s, pr, out, 80, 24)
	host.greet("")

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
	a, host := newSessionShell(context.Background(), s, pr, out, 80, 24)
	host.greet("")

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

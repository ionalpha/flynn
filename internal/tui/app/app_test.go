package app_test

import (
	"bytes"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ionalpha/flynn/internal/tui/app"
	"github.com/ionalpha/flynn/internal/tui/input"
)

// syncBuf is a concurrency-safe output sink: frames arrive from the
// scheduler's goroutine while the test reads from its own.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// shell runs an App over a pipe and a captured output for one test.
type shell struct {
	app      *app.App
	in       *io.PipeWriter
	out      *syncBuf
	done     chan error
	finished chan struct{}
}

// start builds and runs a shell. mut customizes the config before New.
func start(t *testing.T, mut func(*app.Config)) *shell {
	t.Helper()
	pr, pw := io.Pipe()
	out := &syncBuf{}
	cfg := app.Config{
		Input:         pr,
		Output:        out,
		Width:         40,
		Height:        12,
		FrameInterval: time.Millisecond,
		EscDelay:      5 * time.Millisecond,
	}
	if mut != nil {
		mut(&cfg)
	}
	s := &shell{app: app.New(cfg), in: pw, out: out, done: make(chan error, 1), finished: make(chan struct{})}
	go func() {
		s.done <- s.app.Run()
		close(s.finished)
	}()
	t.Cleanup(func() {
		s.app.Quit()
		_ = pw.Close()
		select {
		case <-s.finished:
		case <-time.After(2 * time.Second):
			t.Error("Run did not return")
		}
	})
	return s
}

func (s *shell) press(t *testing.T, bytes string) {
	t.Helper()
	if _, err := io.WriteString(s.in, bytes); err != nil {
		t.Fatalf("write input: %v", err)
	}
}

// eventually polls cond until it holds or the deadline passes.
func eventually(t *testing.T, cond func() bool, fail func()) {
	t.Helper()
	timeout := time.After(2 * time.Second)
	for {
		if cond() {
			return
		}
		select {
		case <-timeout:
			fail()
			return
		case <-time.After(time.Millisecond):
		}
	}
}

// awaitOutput polls until the captured output contains want.
func (s *shell) awaitOutput(t *testing.T, want string) {
	t.Helper()
	eventually(t, func() bool { return strings.Contains(s.out.String(), want) }, func() {
		t.Fatalf("output never contained %q; got:\n%q", want, s.out.String())
	})
}

func (s *shell) awaitDone(t *testing.T) error {
	t.Helper()
	select {
	case err := <-s.done:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return")
		return nil
	}
}

func awaitSubmit(t *testing.T, ch chan string) string {
	t.Helper()
	select {
	case text := <-ch:
		return text
	case <-time.After(2 * time.Second):
		t.Fatal("no submit arrived")
		return ""
	}
}

func expectNoSubmit(t *testing.T, ch chan string) {
	t.Helper()
	select {
	case text := <-ch:
		t.Fatalf("unexpected submit %q", text)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestSubmitDeliversThePromptAndClearsTheComposer(t *testing.T) {
	submits := make(chan string, 1)
	s := start(t, func(c *app.Config) {
		c.OnSubmit = func(text string) { submits <- text }
	})
	s.press(t, "hello spine\r")
	if got := awaitSubmit(t, submits); got != "hello spine" {
		t.Fatalf("submitted %q, want %q", got, "hello spine")
	}
	// The composer cleared: the next Enter submits nothing.
	s.press(t, "\r")
	expectNoSubmit(t, submits)
}

func TestBlankEnterSubmitsNothing(t *testing.T) {
	submits := make(chan string, 1)
	s := start(t, func(c *app.Config) {
		c.OnSubmit = func(text string) { submits <- text }
	})
	s.press(t, "\r   \r")
	expectNoSubmit(t, submits)
}

func TestCtrlCClearsThePromptThenQuits(t *testing.T) {
	submits := make(chan string, 1)
	s := start(t, func(c *app.Config) {
		c.OnSubmit = func(text string) { submits <- text }
	})
	s.press(t, "draft")
	s.awaitOutput(t, "draft")
	// First Ctrl+C clears the prompt but keeps the session alive.
	s.press(t, "\x03\r")
	expectNoSubmit(t, submits)
	// Ctrl+C on the now-empty prompt ends the session.
	s.press(t, "\x03")
	if err := s.awaitDone(t); err != nil {
		t.Fatalf("Run returned %v", err)
	}
}

func TestEOFEndsTheSession(t *testing.T) {
	s := start(t, nil)
	_ = s.in.Close()
	if err := s.awaitDone(t); err != nil {
		t.Fatalf("Run returned %v", err)
	}
}

func TestEscapeReachesTheHook(t *testing.T) {
	escs := make(chan struct{}, 1)
	s := start(t, func(c *app.Config) {
		c.OnEsc = func() { escs <- struct{}{} }
	})
	s.press(t, "\x1b")
	select {
	case <-escs:
	case <-time.After(2 * time.Second):
		t.Fatal("Escape never reached the hook")
	}
}

func TestUnclaimedKeysFallThroughToOnKey(t *testing.T) {
	keys := make(chan input.Key, 1)
	s := start(t, func(c *app.Config) {
		c.OnKey = func(k input.Key) bool {
			keys <- k
			return true
		}
	})
	s.press(t, "\t")
	select {
	case k := <-keys:
		if k.Code != input.KeyTab {
			t.Fatalf("OnKey got %v, want Tab", k)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Tab never reached OnKey")
	}
}

func TestHistoryRecallsThePreviousPrompt(t *testing.T) {
	submits := make(chan string, 2)
	s := start(t, func(c *app.Config) {
		c.OnSubmit = func(text string) { submits <- text }
	})
	s.press(t, "one\r")
	awaitSubmit(t, submits)
	s.press(t, "two\r")
	awaitSubmit(t, submits)
	// Up recalls the newest prompt; Enter resubmits it.
	s.press(t, "\x1b[A\r")
	if got := awaitSubmit(t, submits); got != "two" {
		t.Fatalf("recalled %q, want %q", got, "two")
	}
	// Two steps back reaches the older prompt.
	s.press(t, "\x1b[A\x1b[A\r")
	if got := awaitSubmit(t, submits); got != "one" {
		t.Fatalf("recalled %q, want %q", got, "one")
	}
}

func TestHistoryDownRestoresTheDraft(t *testing.T) {
	submits := make(chan string, 2)
	s := start(t, func(c *app.Config) {
		c.OnSubmit = func(text string) { submits <- text }
	})
	s.press(t, "sent\r")
	awaitSubmit(t, submits)
	// Type a draft, step into history, and come back down: the draft
	// survives the round trip.
	s.press(t, "draft\x1b[A\x1b[B\r")
	if got := awaitSubmit(t, submits); got != "draft" {
		t.Fatalf("draft came back as %q", got)
	}
}

// lines is a fixed-content live component.
type lines []string

func (l lines) Render(int) []string { return l }

func TestLiveAndFinalizedOutputReachTheTerminal(t *testing.T) {
	s := start(t, nil)
	s.app.SetLive(lines{"streaming tokens"})
	s.app.SetStatus("thinking")
	s.app.Append("finalized answer")
	s.awaitOutput(t, "streaming tokens")
	s.awaitOutput(t, "thinking")
	s.awaitOutput(t, "finalized answer")
}

func TestPlaceholderShowsUntilTyping(t *testing.T) {
	s := start(t, func(c *app.Config) { c.Placeholder = "ask anything" })
	s.awaitOutput(t, "ask anything")
	s.press(t, "x")
	s.awaitOutput(t, "x")
}

func TestPasteLandsInTheComposerAsOneUnit(t *testing.T) {
	submits := make(chan string, 1)
	s := start(t, func(c *app.Config) {
		c.OnSubmit = func(text string) { submits <- text }
	})
	s.press(t, "\x1b[200~pasted text\x1b[201~\r")
	if got := awaitSubmit(t, submits); got != "pasted text" {
		t.Fatalf("submitted %q, want the pasted text", got)
	}
}

func TestResizeRepaintsFromScratch(t *testing.T) {
	s := start(t, nil)
	s.press(t, "wide")
	s.awaitOutput(t, "wide")
	before := len(s.out.String())
	s.app.Resize(20, 8)
	// The repaint path clears the live region and redraws it whole.
	eventually(t, func() bool {
		tail := s.out.String()[before:]
		return strings.Contains(tail, "\x1b[J") && strings.Contains(tail, "wide")
	}, func() {
		t.Fatalf("no from-scratch repaint after resize; tail:\n%q", s.out.String()[before:])
	})
}

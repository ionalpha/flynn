package app_test

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ionalpha/flynn/internal/tui/app"
	"github.com/ionalpha/flynn/internal/tui/editor"
	"github.com/ionalpha/flynn/internal/tui/fuzzy"
)

// fakeCompleter serves a fixed universe through the real ranker and records
// what the shell reports back.
type fakeCompleter struct {
	mu       sync.Mutex
	universe []string
	queries  []string
	accepted []string
}

func (f *fakeCompleter) Complete(query string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries = append(f.queries, query)
	return fuzzy.Rank(query, f.universe, 6, nil)
}

func (f *fakeCompleter) Accepted(item string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accepted = append(f.accepted, item)
}

func (f *fakeCompleter) picks() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.accepted...)
}

func startWithCompleter(t *testing.T, universe []string, submits chan string) (*shell, *fakeCompleter) {
	t.Helper()
	fc := &fakeCompleter{universe: universe}
	s := start(t, func(c *app.Config) {
		c.Completer = fc
		if submits != nil {
			c.OnSubmit = func(text string, _ []editor.Attachment) { submits <- text }
		}
	})
	return s, fc
}

// fakeCommands offers a fixed candidate set for any slash-command line, enough to drive
// the command-completion menu mode in tests.
type fakeCommands struct{ cands []app.CommandCandidate }

func (f *fakeCommands) Suggest(line string) []app.CommandCandidate {
	if strings.HasPrefix(strings.TrimLeft(line, " "), "/") {
		return f.cands
	}
	return nil
}

// TestSlashCommandCompletionOpensAndApplies proves typing a slash command opens the
// command menu and accepting a candidate replaces the whole composer line with it.
func TestSlashCommandCompletionOpensAndApplies(t *testing.T) {
	submits := make(chan string, 1)
	fc := &fakeCommands{cands: []app.CommandCandidate{
		{Show: "/model", Apply: "/model "},
		{Show: "/models", Apply: "/models"},
	}}
	s := start(t, func(c *app.Config) {
		c.Commands = fc
		c.OnSubmit = func(text string, _ []editor.Attachment) { submits <- text }
	})
	s.press(t, "/mo")
	s.awaitOutput(t, "/model")
	s.awaitOutput(t, "/models")
	// Tab accepts the highlighted candidate, replacing the line; Enter then submits it.
	s.press(t, "\t\r")
	if got := awaitSubmit(t, submits); got != "/model " {
		t.Fatalf("submitted %q, want %q", got, "/model ")
	}
}

func TestTypingATriggerOpensTheMenu(t *testing.T) {
	s, _ := startWithCompleter(t, []string{"src/main.go", "docs/readme.md"}, nil)
	s.press(t, "@")
	s.awaitOutput(t, "src/main.go")
	s.awaitOutput(t, "docs/readme.md")
}

func TestQueryFiltersTheMenu(t *testing.T) {
	s, _ := startWithCompleter(t, []string{"src/main.go", "docs/readme.md"}, nil)
	s.press(t, "@docs")
	s.awaitOutput(t, "> docs/readme.md")
}

func TestEnterAcceptsTheSelectionInsteadOfSubmitting(t *testing.T) {
	submits := make(chan string, 1)
	s, fc := startWithCompleter(t, []string{"src/main.go"}, submits)
	s.press(t, "read @main\r")
	// The Enter went to the menu, not to submit.
	expectNoSubmit(t, submits)
	s.press(t, "\r")
	if got := awaitSubmit(t, submits); got != "read @src/main.go " {
		t.Fatalf("submitted %q", got)
	}
	eventually(t, func() bool {
		p := fc.picks()
		return len(p) == 1 && p[0] == "src/main.go"
	}, func() {
		t.Fatalf("Accepted never saw the pick; got %v", fc.picks())
	})
}

func TestTabAcceptsTheSelection(t *testing.T) {
	submits := make(chan string, 1)
	s, _ := startWithCompleter(t, []string{"src/main.go"}, submits)
	s.press(t, "@main\t\r")
	if got := awaitSubmit(t, submits); got != "@src/main.go " {
		t.Fatalf("submitted %q", got)
	}
}

func TestArrowsMoveTheSelection(t *testing.T) {
	submits := make(chan string, 1)
	// The empty query lists the whole universe; ranking ties break by
	// length then lexically, so the order is stable.
	s, _ := startWithCompleter(t, []string{"aa.go", "bb.go"}, submits)
	s.press(t, "@")
	s.awaitOutput(t, "> aa.go")
	s.press(t, "\x1b[B")
	s.awaitOutput(t, "> bb.go")
	s.press(t, "\t\r")
	if got := awaitSubmit(t, submits); got != "@bb.go " {
		t.Fatalf("submitted %q", got)
	}
}

func TestEscapeDismissesTheMenuWithoutReachingTheHost(t *testing.T) {
	escapes := make(chan struct{}, 1)
	fc := &fakeCompleter{universe: []string{"src/main.go"}}
	s := start(t, func(c *app.Config) {
		c.Completer = fc
		c.OnEsc = func() { escapes <- struct{}{} }
	})
	s.press(t, "@main")
	s.awaitOutput(t, "> src/main.go")
	s.press(t, "\x1b")
	select {
	case <-escapes:
		t.Fatal("Esc reached OnEsc while the menu was open")
	case <-time.After(50 * time.Millisecond):
	}
	// With the menu closed, Esc belongs to the host again.
	s.press(t, "\x1b")
	select {
	case <-escapes:
	case <-time.After(2 * time.Second):
		t.Fatal("Esc never reached OnEsc after the menu closed")
	}
}

func TestMenuClosesWhenTheTokenGoesAway(t *testing.T) {
	s, _ := startWithCompleter(t, []string{"src/main.go"}, nil)
	s.press(t, "@main")
	s.awaitOutput(t, "> src/main.go")
	s.press(t, " ") // the space ends the token
	eventually(t, func() bool {
		return !strings.Contains(lastFrame(s.out.String()), "> src/main.go")
	}, func() {
		t.Fatalf("menu still visible after the token ended:\n%q", lastFrame(s.out.String()))
	})
}

func TestNoMatchesShowsNoMenu(t *testing.T) {
	submits := make(chan string, 1)
	s, _ := startWithCompleter(t, []string{"src/main.go"}, submits)
	// No candidate matches, so Enter submits the prompt as typed.
	s.press(t, "@zzz\r")
	if got := awaitSubmit(t, submits); got != "@zzz" {
		t.Fatalf("submitted %q", got)
	}
}

func TestMidWordTriggerStaysQuiet(t *testing.T) {
	s, fc := startWithCompleter(t, []string{"src/main.go"}, nil)
	s.press(t, "mail user@main")
	s.awaitOutput(t, "user@main")
	fc.mu.Lock()
	n := len(fc.queries)
	fc.mu.Unlock()
	if n != 0 {
		t.Fatalf("mid-word trigger queried the completer %d times", n)
	}
}

// lastFrame trims the capture to the output after the final synchronized-
// output begin marker, approximating the frame currently on screen.
func lastFrame(out string) string {
	const begin = "\x1b[?2026h"
	if i := strings.LastIndex(out, begin); i >= 0 {
		return out[i:]
	}
	return out
}

package main

import (
	"context"
	"strings"
	"testing"
)

// TestCommandTableRowsAreComplete proves every row of the table is usable by everything
// that reads it: a name to type, a description for /help, and a handler for each of the
// two interfaces. A row missing a handler is the failure the table exists to prevent, a
// command one interface runs and the other sends to the model as a prompt, and it is a
// nil dereference at the keystroke rather than a compile error, so it is checked here.
func TestCommandTableRowsAreComplete(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range sessionCommands() {
		if !strings.HasPrefix(c.name, "/") {
			t.Errorf("command %q does not start with a slash", c.name)
		}
		if c.desc == "" {
			t.Errorf("%s has no description, so /help would list it blank", c.name)
		}
		if c.line == nil {
			t.Errorf("%s has no line handler: the line interface would not run it", c.name)
		}
		if c.tui == nil {
			t.Errorf("%s has no full-screen handler: the shell would send it to the model", c.name)
		}
		for _, spelling := range []string{c.name, c.alias} {
			if spelling == "" {
				continue
			}
			if seen[spelling] {
				t.Errorf("%q names two commands; the first one listed would always win", spelling)
			}
			seen[spelling] = true
		}
	}
}

// TestLookupCommandMatchesHowCommandsAreTyped pins the matching rules both interfaces
// now share: the first word decides, capitalisation does not, an alias runs its command,
// the rest of the line arrives as the argument with its interior spacing intact, and
// anything else is a prompt for the model rather than a command.
func TestLookupCommandMatchesHowCommandsAreTyped(t *testing.T) {
	for _, tc := range []struct {
		line, want, arg string
		ok              bool
	}{
		{line: "/seal", want: "/seal", ok: true},
		{line: "  /seal  ", want: "/seal", ok: true},
		{line: "/SEAL", want: "/seal", ok: true},
		{line: "/seal now please", want: "/seal", arg: "now please", ok: true},
		{line: "?", want: "/help", ok: true},
		{line: "/remember the  build  is  slow", want: "/remember", arg: "the  build  is  slow", ok: true},
		{line: "/model openai:gpt-5.5", want: "/model", arg: "openai:gpt-5.5", ok: true},
		{line: "what does /seal do?", ok: false},
		{line: "/nonsense", ok: false},
		{line: "", ok: false},
		{line: "   ", ok: false},
	} {
		cmd, arg, ok := lookupCommand(tc.line)
		if ok != tc.ok {
			t.Errorf("lookupCommand(%q) claimed = %v, want %v", tc.line, ok, tc.ok)
			continue
		}
		if !ok {
			continue
		}
		if cmd.name != tc.want {
			t.Errorf("lookupCommand(%q) = %s, want %s", tc.line, cmd.name, tc.want)
		}
		if arg != tc.arg {
			t.Errorf("lookupCommand(%q) arg = %q, want %q", tc.line, arg, tc.arg)
		}
	}
}

// TestEveryCommandRunsInLineMode proves the line interface claims every command in the
// table rather than leaking it to the model. It asserts only that the line was claimed:
// most of these commands report that there is nothing to seal or verify yet on a session
// that has not run a turn, and that is the right answer, not a failure.
func TestEveryCommandRunsInLineMode(t *testing.T) {
	for _, c := range sessionCommands() {
		t.Run(c.name, func(t *testing.T) {
			s, _ := newSlashSession(t, constModel{text: "unused"})
			handled, _ := s.replCommand(context.Background(), c.name)
			if !handled {
				t.Fatalf("%s reached the model as a prompt instead of running as a command", c.name)
			}
		})
	}
}

// TestEveryCommandIsListedByHelp proves /help describes the whole table, so the listing
// cannot fall behind the commands the session actually runs.
func TestEveryCommandIsListedByHelp(t *testing.T) {
	var buf strings.Builder
	renderHelp(&buf)
	out := buf.String()
	for _, c := range sessionCommands() {
		if !strings.Contains(out, c.helpKey()) {
			t.Errorf("/help does not list %q:\n%s", c.helpKey(), out)
		}
	}
}

// TestEveryCommandIsOfferedByCompletion proves the composer can complete every command
// in the table, and that the ones taking an argument complete with a trailing space so
// the argument can be typed next.
func TestEveryCommandIsOfferedByCompletion(t *testing.T) {
	isName := func(s string) bool {
		for _, c := range sessionCommands() {
			if c.name == s {
				return true
			}
		}
		return false
	}
	for _, c := range sessionCommands() {
		// Complete a prefix of the name rather than the name itself, which correctly
		// offers nothing so that Enter submits it. Shorten past any prefix that is a
		// command in its own right, which is what /model is to /models.
		partial := c.name[:len(c.name)-1]
		for len(partial) > 1 && isName(partial) {
			partial = partial[:len(partial)-1]
		}
		var found bool
		for _, cand := range commandNames(partial) {
			if cand.Show != c.name {
				continue
			}
			found = true
			want := c.name
			if c.arg != "" {
				want += " "
			}
			if cand.Apply != want {
				t.Errorf("completing %s applies %q, want %q", c.name, cand.Apply, want)
			}
		}
		if !found {
			t.Errorf("completing %q did not offer %s", partial, c.name)
		}
	}
}

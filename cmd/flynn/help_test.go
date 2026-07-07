package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestRenderHelpListsCommandsAndShortcuts checks the shared help text names the
// session commands, the key shortcuts, and the one rendering switch, so both
// front-ends show a complete list.
func TestRenderHelpListsCommandsAndShortcuts(t *testing.T) {
	var buf bytes.Buffer
	renderHelp(&buf)
	out := buf.String()
	for _, want := range []string{"/model", "/models", "/seal", "/fork", "shortcuts:", "ctrl+d", "FLYNN_TUI_ALTSCREEN"} {
		if !strings.Contains(out, want) {
			t.Errorf("help missing %q:\n%s", want, out)
		}
	}
}

// TestShellHelpShowsListing proves /help runs as a command in the full-screen
// session: the listing reaches the scrollback and the input never runs as a turn.
func TestShellHelpShowsListing(t *testing.T) {
	host, ui := newHostForTest(t, constModel{text: "TURN_SHOULD_NOT_RUN"})
	host.submit("/help", nil)
	waitIdle(t, host)

	tr := ui.transcript()
	if !strings.Contains(tr, "commands:") || !strings.Contains(tr, "/model") {
		t.Fatalf("/help did not print the listing to the scrollback:\n%s", tr)
	}
	if strings.Contains(tr, "TURN_SHOULD_NOT_RUN") {
		t.Error("/help leaked to the model as a turn instead of running as a command")
	}
}

// TestShellQuestionMarkShowsHelp proves a bare ? is an alias for /help and is not
// sent to the model.
func TestShellQuestionMarkShowsHelp(t *testing.T) {
	host, ui := newHostForTest(t, constModel{text: "TURN_SHOULD_NOT_RUN"})
	host.submit("?", nil)
	waitIdle(t, host)

	tr := ui.transcript()
	if !strings.Contains(tr, "commands:") {
		t.Fatalf("? did not show the help listing:\n%s", tr)
	}
	if strings.Contains(tr, "TURN_SHOULD_NOT_RUN") {
		t.Error("? leaked to the model as a turn instead of showing help")
	}
}

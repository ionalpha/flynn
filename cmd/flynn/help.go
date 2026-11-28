package main

import (
	"fmt"
	"io"
	"strings"
)

// helpEntry is one row of the /help listing: an invocation and what it does.
type helpEntry struct{ key, desc string }

// helpSection is a titled group of /help rows.
type helpSection struct {
	title   string
	entries []helpEntry
}

// The commands half of the listing is read from sessionCommands, the one table both
// interfaces dispatch through, so /help cannot describe a command the session does not
// run or omit one it does. The shortcuts are keys the composer binds rather than
// commands, so they have no entry there and are listed here.
var sessionShortcuts = []helpEntry{
	{"enter", "send the message"},
	{"alt+enter, ctrl+j", "start a newline in the composer"},
	{"@", "mention a file (completes as you type)"},
	{"!", "run a shell command in the working directory"},
	{"ctrl+o", "toggle the governance panel"},
	{"ctrl+g", "edit the draft in $EDITOR"},
	{"ctrl+v", "paste an image"},
	{"up, down", "walk input history"},
	{"esc, ctrl+c", "cancel the running turn"},
	{"ctrl+d", "quit the session"},
}

// sessionEnvironment lists the variables read when the session starts, which change
// how it draws itself rather than what it does.
var sessionEnvironment = []helpEntry{
	{"FLYNN_TUI_ALTSCREEN=1", "run the session full-screen instead of inline"},
	{"NO_COLOR=1", "draw with attributes only when no theme file is set"},
}

// renderHelp writes the session's commands, keyboard shortcuts and environment
// switches to w. Both front-ends call it so /help shows the same list wherever it
// runs. One key column is shared by every section, so the descriptions line up down
// the whole listing rather than jumping at each title.
func renderHelp(w io.Writer) {
	var cmds []helpEntry
	for _, c := range sessionCommands() {
		cmds = append(cmds, helpEntry{c.helpKey(), c.desc})
	}
	sections := []helpSection{
		{"Commands", cmds},
		{"Shortcuts", sessionShortcuts},
		{"Environment", sessionEnvironment},
	}
	keyWidth := 0
	for _, s := range sections {
		for _, e := range s.entries {
			keyWidth = max(keyWidth, len(e.key))
		}
	}
	for i, s := range sections {
		if i > 0 {
			_, _ = fmt.Fprintln(w)
		}
		_, _ = fmt.Fprintln(w, s.title)
		for _, e := range s.entries {
			_, _ = fmt.Fprintf(w, "  %s%s  %s\n", e.key, strings.Repeat(" ", keyWidth-len(e.key)), e.desc)
		}
	}
}

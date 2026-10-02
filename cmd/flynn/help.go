package main

import (
	"fmt"
	"io"
	"text/tabwriter"
)

// helpEntry is one row of the /help listing: an invocation and what it does.
type helpEntry struct{ key, desc string }

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

// renderHelp writes the session's commands and keyboard shortcuts to w, aligned
// in two columns. Both front-ends call it so /help shows the same list wherever
// it runs. The trailing note points at the one environment switch that changes
// how the session draws itself.
func renderHelp(w io.Writer) {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "commands:")
	for _, c := range sessionCommands() {
		_, _ = fmt.Fprintf(tw, "  %s\t%s\n", c.helpKey(), c.desc)
	}
	_, _ = fmt.Fprintln(tw, "\nshortcuts:")
	for _, e := range sessionShortcuts {
		_, _ = fmt.Fprintf(tw, "  %s\t%s\n", e.key, e.desc)
	}
	_, _ = fmt.Fprintln(tw, "\nrendering:")
	_, _ = fmt.Fprintf(tw, "  %s\t%s\n", "FLYNN_TUI_ALTSCREEN=1", "run the session full-screen instead of inline")
	_ = tw.Flush()
}

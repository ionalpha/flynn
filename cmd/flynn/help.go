package main

import (
	"fmt"
	"io"
	"text/tabwriter"
)

// helpEntry is one row of the /help listing: an invocation and what it does.
type helpEntry struct{ key, desc string }

// sessionCommands and sessionShortcuts back the /help listing. Keeping them in
// one place means /help, the line-based session, and the footer hint never drift
// from what the dispatch actually handles.
var sessionCommands = []helpEntry{
	{"/model [provider:model]", "show the current model, or switch to one and save it as the default"},
	{"/models", "list the model catalog"},
	{"/seal", "seal the run into a verifiable record"},
	{"/verify", "verify the sealed record, tier by tier"},
	{"/export", "write the sealed record to a portable file"},
	{"/fork", "branch the run into a new one, leaving this one untouched"},
	{"/replay", "re-render the run from its record"},
	{"/tokens", "break down this run's token usage"},
	{"/compact", "summarize the conversation to continue with less context"},
	{"/clear", "start a fresh conversation"},
	{"/help, ?", "show this list"},
}

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
	for _, e := range sessionCommands {
		_, _ = fmt.Fprintf(tw, "  %s\t%s\n", e.key, e.desc)
	}
	_, _ = fmt.Fprintln(tw, "\nshortcuts:")
	for _, e := range sessionShortcuts {
		_, _ = fmt.Fprintf(tw, "  %s\t%s\n", e.key, e.desc)
	}
	_, _ = fmt.Fprintln(tw, "\nrendering:")
	_, _ = fmt.Fprintf(tw, "  %s\t%s\n", "FLYNN_TUI_ALTSCREEN=1", "run the session full-screen instead of inline")
	_ = tw.Flush()
}

package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ionalpha/flynn/internal/projectctx"
	"github.com/ionalpha/flynn/internal/text"
	"github.com/ionalpha/flynn/internal/tui/screen"
	"github.com/ionalpha/flynn/internal/tui/theme"
	"github.com/ionalpha/flynn/internal/version"
)

// blockKind is one kind of line in a session transcript. Every renderer of a run
// (the interactive shell, the line-mode session, `flynn goal`, `flynn inspect`)
// lays its lines out through [layBlock], so a tool failure or a cancelled turn
// carries the same mark and indent wherever it is read.
type blockKind int

const (
	blockUser blockKind = iota
	blockShell
	blockToolCall
	blockToolResult
	blockToolError
	blockOutput
	blockRejected
	blockWarning
	blockNotice
	blockError
	blockCancelled
)

// blockStyle is a kind's gutter and roles. The mark fills the two cells before the
// text on the first row and continuation rows are padded to match, so wrapped text
// stays in its column. indent nests a kind under the one it answers: results and
// shell output sit under the call or command that produced them.
type blockStyle struct {
	mark     string
	indent   int
	markRole theme.Role
	role     theme.Role
}

var blockStyles = map[blockKind]blockStyle{
	blockUser:       {"> ", 0, theme.UserPrefix, theme.UserText},
	blockShell:      {"! ", 0, theme.UserPrefix, theme.UserText},
	blockToolCall:   {"→ ", 0, theme.ToolName, theme.ToolDetail},
	blockToolResult: {"← ", 2, theme.ToolOutput, theme.ToolOutput},
	blockToolError:  {"✗ ", 2, theme.Error, theme.Error},
	blockOutput:     {"", 2, theme.ToolOutput, theme.ToolOutput},
	blockRejected:   {"✗ ", 0, theme.Rejected, theme.Rejected},
	blockWarning:    {"! ", 2, theme.Warning, theme.Warning},
	blockNotice:     {"· ", 0, theme.Notice, theme.Notice},
	blockError:      {"✗ ", 0, theme.Error, theme.Error},
	blockCancelled:  {"○ ", 0, theme.Cancelled, theme.Cancelled},
}

// paint styles s with role, or returns it as is when th is nil: the plain renderers
// share the layout and leave out the escape codes.
func paint(th *theme.Theme, role theme.Role, s string) string {
	if th == nil {
		return s
	}
	return th.Render(role, s)
}

// layBlock lays s out as one block of kind k, wrapped so no row is wider than
// width (no wrapping when width is 0 or less). The text passes through
// [text.Inert] first: it comes from the model, a tool or a subprocess, and a raw
// escape byte reaching the terminal would let any of them repaint the screen.
func layBlock(th *theme.Theme, k blockKind, s string, width int) []string {
	st := blockStyles[k]
	lead := strings.Repeat(" ", st.indent)
	pad := lead + strings.Repeat(" ", screen.Width(st.mark))
	body := strings.TrimRight(text.Inert(s), "\n")
	if width > 0 {
		w := width - screen.Width(pad)
		if w < 1 {
			w = 1
		}
		body = screen.Wrap(body, w)
	}
	rows := strings.Split(body, "\n")
	out := make([]string, len(rows))
	for i, r := range rows {
		head := pad
		if i == 0 {
			head = lead + paint(th, st.markRole, st.mark)
		}
		out[i] = head + paint(th, st.role, strings.TrimRight(r, " "))
	}
	return out
}

// layRow is [layBlock] held to one row: whitespace runs collapse and s is cut to
// fit rather than wrapped, for a summary that should not grow the transcript.
func layRow(th *theme.Theme, k blockKind, s string, width int) string {
	st := blockStyles[k]
	s = strings.Join(strings.Fields(text.Inert(s)), " ")
	if width > 0 {
		room := width - st.indent - screen.Width(st.mark)
		if room > 3 && screen.Width(s) > room {
			s = strings.TrimRight(screen.Truncate(s, room-3), " ") + "..."
		}
	}
	return layBlock(th, k, s, 0)[0]
}

// toolHead is the one-row header of a tool call: the mark, the tool's name and a
// short summary of its argument, cut to width so a long command never wraps the
// header onto a second row.
func toolHead(th *theme.Theme, name, detail string, width int) string {
	st := blockStyles[blockToolCall]
	name = text.Inert(name)
	detail = text.Inert(detail)
	if width > 0 && detail != "" {
		room := width - screen.Width(st.mark+name+" ")
		switch {
		case room < 4:
			detail = ""
		case screen.Width(detail) > room:
			detail = strings.TrimRight(screen.Truncate(detail, room-3), " ") + "..."
		}
	}
	head := paint(th, st.markRole, st.mark+name)
	if detail != "" {
		head += " " + paint(th, st.role, detail)
	}
	return head
}

// resultSummary is the one line a successful tool result gets: its first non-blank
// line, and how many more there were. The full output stays in the record; the
// transcript shows enough to see that the call did something.
func resultSummary(result string) string {
	var first string
	more := 0
	for _, l := range strings.Split(strings.TrimRight(result, "\n"), "\n") {
		if first == "" {
			first = strings.TrimSpace(l)
			continue
		}
		more++
	}
	if first == "" {
		return "(no output)"
	}
	first = text.Clip(first, 100)
	if more > 0 {
		first += " (+" + strconv.Itoa(more) + " lines)"
	}
	return first
}

// sessionBanner is the opening of an interactive session, the same three rows in
// the full-screen and line interfaces: the build and model, where the session runs
// and whether the repository's instruction files are in the prompt, and where to
// find the commands.
func sessionBanner(th *theme.Theme, model, cwd string, project projectctx.Decision) []string {
	return []string{
		paint(th, theme.Strong, "flynn "+version.String()) + paint(th, theme.Muted, " · model ") + text.Inert(model),
		paint(th, theme.Muted, shortPath(cwd)+" · "+projectState(project)),
		paint(th, theme.Muted, "/help for commands"),
	}
}

// projectState is the banner's account of the instruction files: none found, or
// which ones and whether they were loaded.
func projectState(d projectctx.Decision) string {
	if d.Set.Empty() {
		return "no project files"
	}
	names := strings.Join(d.Set.Paths(), ", ")
	if d.Verdict == projectctx.Loaded {
		return "project files loaded: " + names
	}
	return "project files skipped: " + names
}

// shortPath writes a path under the home directory as ~/..., the form a person
// types, so a deep working directory still fits on the banner row.
func shortPath(p string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	return shortPathUnder(p, home)
}

func shortPathUnder(p, home string) string {
	rel, err := filepath.Rel(home, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return p
	}
	if rel == "." {
		return "~"
	}
	return "~" + string(filepath.Separator) + rel
}

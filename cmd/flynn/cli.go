package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	tuiterm "github.com/ionalpha/flynn/internal/tui/term"
)

// cliCommand is one command or subcommand of the binary. [cliCommands] is the one list
// the router dispatches from and the help pages render, so a command cannot be routed
// without a page, and a page cannot name a command that does not run.
type cliCommand struct {
	name    string
	aliases []string
	group   string
	// summary is the one line the command's row shows in a listing.
	summary string
	// usage is the synopsis, one form per line, without the leading "flynn ".
	usage []string
	// long is the page's description: paragraphs split on blank lines. A paragraph whose
	// lines start with a space is printed as written, so an example keeps its layout.
	long string
	// flags are the command's own flags, for the page.
	flags []cliFlag
	// runFlags names root flags the command reads; their rows come from the parsed flag
	// set, so the text cannot drift from what the flag package accepts.
	runFlags []string
	examples []string
	subs     []cliCommand
	// invoke runs a command that needs the model, the streams or the run flags; local
	// runs one whose only state is the data directory. A subcommand has neither, because
	// its parent dispatches it.
	invoke func(inv invocation, rest []string) int
	local  func(args []string, dataDir string) error
}

// cliFlag is a command's own flag, as its page shows it.
type cliFlag struct{ name, short, value, usage string }

// The groups of the top-level listing, by what the reader wants to do, in page order.
const (
	groupWork         = "Start and drive work"
	groupInspect      = "Inspect runs and the record"
	groupModels       = "Models"
	groupIntegrations = "Integrations and extensions"
	groupMemory       = "Memory and skills"
	groupService      = "Run as a service"
	groupMaintenance  = "Maintenance"
)

var cliGroups = []string{groupWork, groupInspect, groupModels, groupIntegrations, groupMemory, groupService, groupMaintenance}

// rootFlagGroups sorts the root flags for `flynn help flags`. A test fails when a flag
// is registered without a group, so a new flag cannot be left off the page.
var rootFlagGroups = []struct {
	title, note string
	names       []string
}{
	{"Model and state", "", []string{"model", "data-dir", "no-learn", "no-bundled-skills", "project-files"}},
	{
		"Run controls",
		"These govern a run started by `flynn goal`, a session or `flynn serve`, and a command that would ignore one refuses it. 'flynn help goal' says how --require-approval, --irreversible and --allow relate to a run's grant.",
		[]string{"verify", "require-proof", "goal-spec", "require-approval", "irreversible", "allow", "fanout", "max-cost", "max-tokens"},
	},
	{
		"Shell",
		"These bound the commands a run's agent executes. By default a command has no network and cannot read the data directory or credential locations; Windows cannot enforce either, which the --require flags turn into a refusal. Each choice is recorded on the run's stream.",
		[]string{"max-memory", "max-processes", "allow-network", "allow-credential-reads", "require-credential-hiding", "require-network-denial"},
	},
	{
		"Selection",
		"These choose what a session, `flynn goal`, `flynn resume` or `flynn watch` offers a run. Each can also be set in selection.json in the data directory; a flag replaces the file's value, and the deny lists are joined. Nothing is deleted: a later run without the flag is offered everything again.",
		[]string{"bare", "skills", "no-skill", "tools", "no-tools"},
	},
	{"Output", "", []string{"v", "plain", "version"}},
	{"Diagnostics", "", []string{"profile", "profile-contention", "leak-watch", "leak-watch-repeat"}},
}

// globalFlagNames are the root flags the top-level page lists; the rest are one
// `flynn help flags` away.
var globalFlagNames = []string{"model", "data-dir", "v", "version"}

// findCommand returns the command in cmds that word names or aliases, or nil.
func findCommand(cmds []cliCommand, word string) *cliCommand {
	for i := range cmds {
		if cmds[i].name == word || slices.Contains(cmds[i].aliases, word) {
			return &cmds[i]
		}
	}
	return nil
}

// findCLI returns the top-level command word names, or nil.
func findCLI(word string) *cliCommand { return findCommand(cliCommands(), word) }

// lookupPath walks a command path ("models fetch"). It returns the command and nil, or
// nil and the usage error naming the first word that is not a command where it stands.
func lookupPath(words []string) (*cliCommand, error) {
	cmds := cliCommands()
	var cmd *cliCommand
	for i, w := range words {
		next := findCommand(cmds, w)
		if next == nil {
			return nil, unknownCommand(strings.Join(words[:i], " "), w)
		}
		cmd, cmds = next, next.subs
	}
	return cmd, nil
}

func isHelpFlag(arg string) bool { return arg == "-h" || arg == "--help" || arg == "-help" }

// helpTarget reports whether a command line asks for help, and the command path the page
// is for. The flag package stops at the first non-flag word, so a help flag after a
// command reaches here unseen, and answering it first keeps it from becoming an
// objective or running the command it asked about. Only the argument right after a
// command or subcommand counts: an objective that mentions "-h" is still an objective.
func helpTarget(rest []string) ([]string, bool) {
	if len(rest) == 0 {
		return nil, false
	}
	if rest[0] == "help" {
		// `flynn help -h` asks about help itself, and `flynn help goal -h` about goal.
		topic := rest[1:]
		if n := len(topic); n > 0 && isHelpFlag(topic[n-1]) {
			topic = topic[:n-1]
			if len(topic) == 0 {
				topic = rest[:1]
			}
		}
		return topic, true
	}
	cmd := findCLI(rest[0])
	if cmd == nil || len(rest) < 2 {
		return nil, false
	}
	if isHelpFlag(rest[1]) {
		return rest[:1], true
	}
	if findCommand(cmd.subs, rest[1]) != nil && len(rest) >= 3 && isHelpFlag(rest[2]) {
		return rest[:2], true
	}
	return nil, false
}

// showHelp writes the page for topic to stdout and returns 0, or reports an unknown
// topic on stderr and returns 2. It reads the registry and the flag set only: help opens
// no store, resolves no model and writes nothing, so asking how to use a command can
// never do what the command does.
func showHelp(stdout, stderr io.Writer, fs *flag.FlagSet, topic []string) int {
	switch {
	case len(topic) == 0:
		writeTopHelp(stdout, fs)
		return 0
	case len(topic) == 1 && topic[0] == "flags":
		writeFlagsHelp(stdout, fs)
		return 0
	}
	cmd, err := lookupPath(topic)
	if err != nil {
		return reportError(stderr, err)
	}
	writeCommandHelp(stdout, fs, strings.Join(topic, " "), cmd)
	return 0
}

// helpWidth is the width a page wraps to: the terminal's when w is one, clamped so a very
// wide window still gets readable lines, else 80.
func helpWidth(w io.Writer) int {
	f, ok := w.(*os.File)
	if !ok {
		return 80
	}
	cols, _, err := tuiterm.Size(int(f.Fd()))
	if err != nil || cols <= 0 {
		return 80
	}
	return min(max(cols, 40), 100)
}

// page accumulates a help page, so it is written in one call.
type page struct {
	b     strings.Builder
	width int
}

func (p *page) line(s string) { p.b.WriteString(s + "\n") }

// para writes text as paragraphs wrapped to the page width at indent.
func (p *page) para(text string, indent int) {
	for i, block := range strings.Split(strings.TrimSpace(text), "\n\n") {
		if i > 0 {
			p.line("")
		}
		if strings.HasPrefix(block, " ") {
			for _, l := range strings.Split(block, "\n") {
				p.line(strings.Repeat(" ", indent) + l)
			}
			continue
		}
		for _, l := range wrapWords(block, p.width-indent) {
			p.line(strings.Repeat(" ", indent) + l)
		}
	}
}

// rows writes a two-column list: the left column padded to the widest entry (up to
// 30 columns, past which the description starts on the next line), the right wrapped.
func (p *page) rows(rows [][2]string) {
	col := 0
	for _, r := range rows {
		if len(r[0]) <= 30 {
			col = max(col, len(r[0]))
		}
	}
	col += 4 // two of indent, two of gap
	for _, r := range rows {
		desc := wrapWords(r[1], max(p.width-col, 20))
		left := "  " + r[0]
		if len(left)+2 > col {
			p.line(left)
		} else {
			first := ""
			if len(desc) > 0 {
				first, desc = desc[0], desc[1:]
			}
			p.line(strings.TrimRight(left+strings.Repeat(" ", col-len(left))+first, " "))
		}
		for _, d := range desc {
			p.line(strings.Repeat(" ", col) + d)
		}
	}
}

// wrapWords breaks text into lines of at most width runes, breaking only at spaces; a
// word longer than width gets a line of its own.
func wrapWords(text string, width int) []string {
	var lines []string
	cur := ""
	for _, w := range strings.Fields(text) {
		switch {
		case cur == "":
			cur = w
		case len([]rune(cur))+1+len([]rune(w)) <= width:
			cur += " " + w
		default:
			lines = append(lines, cur)
			cur = w
		}
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	return lines
}

func writeTopHelp(w io.Writer, fs *flag.FlagSet) {
	p := &page{width: helpWidth(w)}
	p.line("Flynn is an autonomous software agent whose runs are governed and recorded.")
	p.line("")
	p.line("Usage:")
	p.line("  flynn [flags] <command> [args]")
	p.line("  flynn [flags]")
	p.line("")
	p.para("With no command, flynn starts an interactive session in a terminal.", 0)
	cmds := cliCommands()
	for _, g := range cliGroups {
		p.line("")
		p.line(g + ":")
		var rows [][2]string
		for _, c := range cmds {
			if c.group == g {
				rows = append(rows, [2]string{c.name, c.summary})
			}
		}
		p.commandRows(rows)
	}
	p.line("")
	p.line("Global flags:")
	var rows [][2]string
	for _, n := range globalFlagNames {
		rows = append(rows, rootFlagRow(fs, n))
	}
	p.rows(append(rows, helpFlagRow))
	p.line("")
	p.line("Run 'flynn help <command>' for a command's usage, flags and examples.")
	p.line("Run 'flynn help flags' for every flag, and 'flynn help goal' for goal specs.")
	_, _ = io.WriteString(w, p.b.String())
}

// commandNameWidth is the column the top-level summaries start at: the longest command
// name plus indent and gap. A test holds every summary to one line at 80 columns from it.
func commandNameWidth() int {
	n := 0
	for _, c := range cliCommands() {
		n = max(n, len(c.name))
	}
	return n + 4
}

// commandRows writes command rows with the summaries aligned at one column on every page
// section, so the listing reads as one table.
func (p *page) commandRows(rows [][2]string) {
	col := commandNameWidth()
	for _, r := range rows {
		left := "  " + r[0]
		desc := wrapWords(r[1], max(p.width-col, 20))
		first := ""
		if len(desc) > 0 {
			first, desc = desc[0], desc[1:]
		}
		p.line(strings.TrimRight(left+strings.Repeat(" ", max(col-len(left), 2))+first, " "))
		for _, d := range desc {
			p.line(strings.Repeat(" ", col) + d)
		}
	}
}

var helpFlagRow = [2]string{"-h, --help", "show help for flynn or for a command"}

func writeCommandHelp(w io.Writer, fs *flag.FlagSet, path string, c *cliCommand) {
	p := &page{width: helpWidth(w)}
	p.para(c.summary+".", 0)
	p.line("")
	p.line("Usage:")
	for _, u := range c.usage {
		p.line("  flynn " + u)
	}
	if c.long != "" {
		p.line("")
		p.para(c.long, 0)
	}
	if len(c.aliases) > 0 {
		p.line("")
		p.line("Aliases:")
		p.line("  " + strings.Join(c.aliases, ", "))
	}
	if len(c.subs) > 0 {
		p.line("")
		p.line("Commands:")
		var rows [][2]string
		for _, s := range c.subs {
			rows = append(rows, [2]string{s.name, s.summary})
		}
		p.rows(rows)
	}
	if len(c.flags) > 0 || len(c.runFlags) > 0 {
		p.line("")
		p.line("Flags:")
		var rows [][2]string
		for _, f := range c.flags {
			rows = append(rows, staticFlagRow(f))
		}
		for _, n := range c.runFlags {
			rows = append(rows, rootFlagRow(fs, n))
		}
		p.rows(rows)
	}
	if len(c.examples) > 0 {
		p.line("")
		p.line("Examples:")
		for _, e := range c.examples {
			p.line("  " + e)
		}
	}
	p.line("")
	if len(c.subs) > 0 {
		p.line(fmt.Sprintf("Run 'flynn help %s <command>' for a subcommand's usage and flags.", path))
	}
	p.line("Run 'flynn help flags' for the flags every command accepts.")
	_, _ = io.WriteString(w, p.b.String())
}

func writeFlagsHelp(w io.Writer, fs *flag.FlagSet) {
	p := &page{width: helpWidth(w)}
	p.para("Flags that apply to every command. They go before the command, as in flynn [flags] <command>; `flynn goal` also reads them between goal and the objective.", 0)
	for _, g := range rootFlagGroups {
		p.line("")
		p.line(g.title + ":")
		if g.note != "" {
			p.para(g.note, 2)
		}
		var rows [][2]string
		for _, n := range g.names {
			rows = append(rows, rootFlagRow(fs, n))
		}
		p.rows(rows)
	}
	_, _ = io.WriteString(w, p.b.String())
}

func staticFlagRow(f cliFlag) [2]string {
	left := "--" + f.name
	if f.short != "" {
		left = "-" + f.short + ", " + left
	}
	if f.value != "" {
		left += " <" + f.value + ">"
	}
	return [2]string{left, f.usage}
}

// rootFlagRow renders a root flag from fs, reading the value name from the backticked
// word in its usage. -v and --verbose are one row, because they are one switch.
func rootFlagRow(fs *flag.FlagSet, name string) [2]string {
	f := fs.Lookup(name)
	if f == nil {
		return [2]string{"--" + name, ""}
	}
	value, usage := flag.UnquoteUsage(f)
	left := "--" + name
	if name == "v" {
		left = "-v, --verbose"
	}
	if value != "" {
		left += " <" + value + ">"
	}
	// The data directory's default is a path on this machine, and the page reads the
	// same on every machine; its usage says where it is.
	if d := f.DefValue; d != "" && d != "0" && d != "false" && name != "data-dir" {
		usage += fmt.Sprintf(" (default %q)", d)
	}
	return [2]string{left, usage}
}

// usageError is a command line that cannot be run as written. It is reported with the
// command's synopsis and exits 2, which separates "you typed it wrong" from a command
// that ran and failed. It is a comparable value, so a fixed one (errMemoryUsage) still
// matches with errors.Is.
type usageError struct {
	// path is the command the usage belongs to ("models fetch"), "" for the top level,
	// or "flags" for a root flag.
	path string
	msg  string
	// unknown is the word that is not a command at path.
	unknown string
}

// usageErr is a refusal that only needs the synopsis of path.
func usageErr(path string) error { return usageError{path: path} }

// usageErrf is a refusal with a message, followed by the synopsis of path.
func usageErrf(path, format string, a ...any) error {
	return usageError{path: path, msg: fmt.Sprintf(format, a...)}
}

// unknownCommand refuses word as a subcommand of path ("" for the top level).
func unknownCommand(path, word string) error { return usageError{path: path, unknown: word} }

// asUsage reports err as a usage error of path, keeping one that already is.
func asUsage(path string, err error) error {
	var ue usageError
	if err == nil || errors.As(err, &ue) {
		return err
	}
	return usageErrf(path, "%v", err)
}

// subUsage refuses a command group given no subcommand, or one it does not have.
func subUsage(path string, args []string) error {
	if len(args) == 0 {
		return usageErr(path)
	}
	return unknownCommand(path, args[0])
}

func (e usageError) Error() string {
	if e.unknown != "" {
		return fmt.Sprintf("unknown command %q for %q", e.unknown, commandPrefix(e.path))
	}
	parts := []string{}
	if e.msg != "" {
		parts = append(parts, e.msg)
	}
	if syn := synopsis(e.path); syn != "" {
		parts = append(parts, syn)
	}
	return strings.Join(parts, "\n")
}

func commandPrefix(path string) string { return strings.TrimSpace("flynn " + path) }

// synopsis is the "usage:" block for path, or "" when path is not a command.
func synopsis(path string) string {
	if path == "" || path == "flags" {
		return ""
	}
	c, err := lookupPath(strings.Fields(path))
	if err != nil || len(c.usage) == 0 {
		return ""
	}
	return "usage: flynn " + strings.Join(c.usage, "\n       flynn ")
}

// write reports e the way every usage error is reported: what was wrong, how the command
// is written, and where its page is.
func (e usageError) write(w io.Writer) {
	help := strings.TrimSpace("flynn help " + e.path)
	if e.unknown != "" {
		_, _ = fmt.Fprintf(w, "%s: unknown command %q\n", commandPrefix(e.path), e.unknown)
		if s := suggestCommand(e.path, e.unknown); s != "" {
			_, _ = fmt.Fprintf(w, "Did you mean '%s %s'?\n", commandPrefix(e.path), s)
		}
		_, _ = fmt.Fprintf(w, "Run '%s' for a list of commands.\n", help)
		return
	}
	if e.msg != "" {
		_, _ = fmt.Fprintln(w, "error: "+e.msg)
	}
	if syn := synopsis(e.path); syn != "" {
		_, _ = fmt.Fprintln(w, syn)
	}
	_, _ = fmt.Fprintf(w, "Run '%s' for details.\n", help)
}

// reportError writes err to w and returns the exit code it maps to: 0 for nil, 2 for a
// [usageError], and 1 for anything else. An error with no message reports an outcome
// through the exit code alone (`flynn version check` exits 1 when an upgrade waits).
func reportError(w io.Writer, err error) int {
	if err == nil {
		return 0
	}
	var ue usageError
	if errors.As(err, &ue) {
		ue.write(w)
		return 2
	}
	if err.Error() != "" {
		_, _ = fmt.Fprintln(w, "error:", err)
	}
	return 1
}

// warn reports something that went wrong without failing the command.
func warn(w io.Writer, format string, a ...any) {
	_, _ = fmt.Fprintf(w, "warning: "+format+"\n", a...)
}

// suggestCommand is the command at path closest to the mistyped word, or "". It reads
// only the registry's names, so a suggestion can never run anything.
func suggestCommand(path, word string) string {
	cmds := cliCommands()
	if path != "" {
		c, err := lookupPath(strings.Fields(path))
		if err != nil {
			return ""
		}
		cmds = c.subs
	}
	return closestName(word, cmds)
}

// closestName returns the name or alias in cmds that word is a prefix of, or that is
// within a small edit distance of it, taking the first in registry order on a tie. The
// distance allowed grows with the word, so a two-letter typo never suggests anything.
func closestName(word string, cmds []cliCommand) string {
	word = strings.ToLower(word)
	limit := max(1, min(2, len(word)/3))
	best, bestDist := "", limit+1
	for _, c := range cmds {
		for _, n := range append([]string{c.name}, c.aliases...) {
			d := editDistance(word, n)
			if d > 1 && len(word) >= 3 && strings.HasPrefix(n, word) {
				d = 1
			}
			if d < bestDist {
				best, bestDist = n, d
			}
		}
	}
	return best
}

// maxSuggestRunes bounds the words [editDistance] compares. The word comes from the
// command line, and the table is the product of the two lengths, so an argument of
// megabytes would otherwise allocate in proportion; no command name comes near it.
const maxSuggestRunes = 64

// editDistance is the optimal string alignment distance: insertions, deletions,
// substitutions and the swap of two adjacent letters each cost one, since a swap is
// the commonest typo. A word over [maxSuggestRunes] is no near miss of any name, so
// it gets a distance past every limit without the table being built.
func editDistance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	if len(ra) > maxSuggestRunes || len(rb) > maxSuggestRunes {
		return maxSuggestRunes + 1
	}
	d := make([][]int, len(ra)+1)
	for i := range d {
		d[i] = make([]int, len(rb)+1)
		d[i][0] = i
	}
	for j := range d[0] {
		d[0][j] = j
	}
	for i := 1; i <= len(ra); i++ {
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			d[i][j] = min(d[i-1][j]+1, d[i][j-1]+1, d[i-1][j-1]+cost)
			if i > 1 && j > 1 && ra[i-1] == rb[j-2] && ra[i-2] == rb[j-1] {
				d[i][j] = min(d[i][j], d[i-2][j-2]+1)
			}
		}
	}
	return d[len(ra)][len(rb)]
}

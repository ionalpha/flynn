package main

import (
	"bytes"
	"errors"
	"flag"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

var updateGolden = flag.Bool("update", false, "rewrite the help goldens in testdata")

// rootFlagSet is the root flag set exactly as run registers it, so a page rendered in a
// test reads the same flags the binary parses.
func rootFlagSet(t *testing.T) *flag.FlagSet {
	t.Helper()
	fs := flag.NewFlagSet("flynn", flag.ContinueOnError)
	run(fs, []string{"help"}, io.Discard, io.Discard)
	return fs
}

// TestRegistryIsConsistent: every entry the router reads can run, its names are unique
// across the level it sits at, and it sits in a group the top-level page prints.
func TestRegistryIsConsistent(t *testing.T) {
	var check func(prefix string, cmds []cliCommand, top bool)
	check = func(prefix string, cmds []cliCommand, top bool) {
		seen := map[string]bool{}
		for _, c := range cmds {
			path := strings.TrimSpace(prefix + " " + c.name)
			for _, n := range append([]string{c.name}, c.aliases...) {
				if seen[n] {
					t.Errorf("%q is used twice under %q", n, prefix)
				}
				seen[n] = true
			}
			if c.summary == "" || len(c.usage) == 0 {
				t.Errorf("%s has no summary or no usage", path)
			}
			if strings.HasSuffix(c.summary, ".") {
				t.Errorf("%s: the summary %q ends in a full stop; the page adds one", path, c.summary)
			}
			for _, u := range c.usage {
				if !strings.HasPrefix(u, path) && !slices.ContainsFunc(c.aliases, func(a string) bool {
					return strings.HasPrefix(u, strings.TrimSpace(prefix+" "+a))
				}) {
					t.Errorf("%s: usage %q does not start with the command", path, u)
				}
			}
			if top {
				if !slices.Contains(cliGroups, c.group) {
					t.Errorf("%s is in group %q, which the top-level page does not print", path, c.group)
				}
				runs := c.invoke != nil || c.local != nil
				if runs == (c.name == "help") {
					t.Errorf("%s: invoke or local must be set on every command but help", path)
				}
				if c.invoke != nil && c.local != nil {
					t.Errorf("%s sets both invoke and local", path)
				}
			} else if c.invoke != nil || c.local != nil || c.group != "" {
				t.Errorf("%s is a subcommand and sets a group or a handler its parent owns", path)
			}
			check(path, c.subs, false)
		}
	}
	check("", cliCommands(), true)
}

// TestSummariesFitOneLine: a summary that wraps on an 80-column terminal breaks the
// aligned listing the top-level page is for.
func TestSummariesFitOneLine(t *testing.T) {
	col := commandNameWidth()
	var walk func(prefix string, cmds []cliCommand)
	walk = func(prefix string, cmds []cliCommand) {
		for _, c := range cmds {
			if n := col + len(c.summary); n > 80 {
				t.Errorf("%s %s: summary is %d columns wide at 80 (%q)", prefix, c.name, n, c.summary)
			}
			walk(prefix+" "+c.name, c.subs)
		}
	}
	walk("flynn", cliCommands())
}

// TestEveryRootFlagHasOneGroupAndOneLine: `flynn help flags` lists the groups, so a flag
// outside every group would be accepted and never documented. A flag row is one line, so
// an explanation longer than that belongs on a command page.
func TestEveryRootFlagHasOneGroupAndOneLine(t *testing.T) {
	fs := rootFlagSet(t)
	count := map[string]int{}
	for _, g := range rootFlagGroups {
		for _, n := range g.names {
			count[n]++
			if fs.Lookup(n) == nil {
				t.Errorf("group %q names --%s, which is not registered", g.title, n)
			}
		}
	}
	for _, n := range globalFlagNames {
		if fs.Lookup(n) == nil {
			t.Errorf("the global flags list --%s, which is not registered", n)
		}
	}
	fs.VisitAll(func(f *flag.Flag) {
		if f.Name == "verbose" {
			return // the -v row shows both spellings of the one switch
		}
		if count[f.Name] != 1 {
			t.Errorf("--%s is in %d flag groups, want 1", f.Name, count[f.Name])
		}
		if strings.Contains(f.Usage, "\n") || len(f.Usage) > 70 {
			t.Errorf("--%s usage is %d characters or several lines; keep a row to one line of 70: %q", f.Name, len(f.Usage), f.Usage)
		}
	})
	for _, c := range cliCommands() {
		for _, n := range c.runFlags {
			if fs.Lookup(n) == nil {
				t.Errorf("%s lists --%s, which is not registered", c.name, n)
			}
		}
	}
}

// TestUsageErrorPathsAreCommands: a usage error renders the synopsis of the path it
// names, so a path the registry does not have would print an error with no usage line.
// The paths are read from the source, so a new call site is checked as it is written.
func TestUsageErrorPathsAreCommands(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	calls := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			id, ok := call.Fun.(*ast.Ident)
			if !ok || !slices.Contains([]string{"usageErr", "usageErrf", "unknownCommand", "subUsage", "asUsage"}, id.Name) {
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			path, _ := strconv.Unquote(lit.Value)
			calls++
			if path == "" || path == "flags" {
				return true
			}
			if _, err := lookupPath(strings.Fields(path)); err != nil {
				t.Errorf("%s: %s(%q) names a path the registry does not have", fset.Position(call.Pos()), id.Name, path)
			}
			return true
		})
	}
	if calls < 20 {
		t.Fatalf("found %d usage-error call sites; the scan is not reading the package", calls)
	}
}

func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *updateGolden {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to write it)", err)
	}
	if got != strings.ReplaceAll(string(want), "\r\n", "\n") {
		t.Errorf("%s changed; run `go test ./cmd/flynn -run Golden -update` and review the diff.\ngot:\n%s", name, got)
	}
}

func TestTopHelpGolden(t *testing.T) {
	var buf bytes.Buffer
	writeTopHelp(&buf, rootFlagSet(t))
	checkGolden(t, "help_top.golden", buf.String())
}

func TestGoalHelpGolden(t *testing.T) {
	checkGolden(t, "help_goal.golden", commandPage(t, "goal"))
}

// TestTopHelpLinesFitTheWidth: the page renders at 80 columns when stdout is not a
// terminal, and every line must fit it.
func TestTopHelpLinesFitTheWidth(t *testing.T) {
	fs := rootFlagSet(t)
	pages := map[string]string{}
	var buf bytes.Buffer
	writeTopHelp(&buf, fs)
	pages["flynn help"] = buf.String()
	buf.Reset()
	writeFlagsHelp(&buf, fs)
	pages["flynn help flags"] = buf.String()
	for _, path := range helpPaths() {
		pages["flynn help "+strings.Join(path, " ")] = commandPage(t, path...)
	}
	for name, page := range pages {
		for _, line := range strings.Split(page, "\n") {
			if len([]rune(line)) > 80 && !strings.HasPrefix(line, "   ") {
				t.Errorf("%s: line is %d columns: %q", name, len([]rune(line)), line)
			}
		}
	}
}

func TestClosestName(t *testing.T) {
	cmds := cliCommands()
	for _, c := range []struct{ word, want string }{
		{"modls", "models"},
		{"mdoels", "models"},
		{"stauts", "status"},
		{"goa", "goal"},
		{"memroy", "memory"},
		{"insp", "inspect"},
		{"xz", ""},
		{"bogus", ""},
		{"zzzzzzzzzz", ""},
	} {
		if got := closestName(c.word, cmds); got != c.want {
			t.Errorf("closestName(%q) = %q, want %q", c.word, got, c.want)
		}
	}
	if got := closestName("fetc", findCLI("models").subs); got != "fetch" {
		t.Errorf("closestName(fetc) under models = %q, want fetch", got)
	}
}

// TestUnknownCommandIsAUsageError: a mistyped command exits 2 on stderr, names the word,
// suggests the command it is closest to, and points at the listing, before anything opens
// the data directory.
func TestUnknownCommandIsAUsageError(t *testing.T) {
	for _, c := range []struct {
		args    []string
		want    []string
		without string
	}{
		{[]string{"bogus"}, []string{`flynn: unknown command "bogus"`, "Run 'flynn help' for a list of commands."}, "Did you mean"},
		{[]string{"modls"}, []string{`flynn: unknown command "modls"`, "Did you mean 'flynn models'?", "Run 'flynn help' for a list of commands."}, ""},
		{[]string{"help", "modls"}, []string{"Did you mean 'flynn models'?"}, ""},
		{[]string{"memory", "lss"}, []string{`flynn memory: unknown command "lss"`, "Did you mean 'flynn memory ls'?", "Run 'flynn help memory' for a list of commands."}, ""},
	} {
		dir := t.TempDir()
		got := runCLIIn(t, dir, c.args...)
		if got.code != 2 || got.stdout != "" {
			t.Errorf("flynn %v: exit %d, stdout %q; want 2 and nothing on stdout", c.args, got.code, got.stdout)
		}
		for _, w := range c.want {
			if !strings.Contains(got.stderr, w) {
				t.Errorf("flynn %v: stderr %q does not contain %q", c.args, got.stderr, w)
			}
		}
		if c.without != "" && strings.Contains(got.stderr, c.without) {
			t.Errorf("flynn %v: stderr %q should not contain %q", c.args, got.stderr, c.without)
		}
	}
}

// TestUsageErrorShowsTheSynopsis: a command given the wrong arguments says what was wrong,
// how the command is written and where its page is, and exits 2.
func TestUsageErrorShowsTheSynopsis(t *testing.T) {
	got := runCLIIn(t, t.TempDir(), "steer")
	if got.code != 2 {
		t.Fatalf("exit %d, want 2; stderr %q", got.code, got.stderr)
	}
	for _, w := range []string{"usage: flynn steer", "Run 'flynn help steer' for details."} {
		if !strings.Contains(got.stderr, w) {
			t.Errorf("stderr %q does not contain %q", got.stderr, w)
		}
	}
}

// TestReportError maps each kind of error to its exit code and output.
func TestReportError(t *testing.T) {
	var buf bytes.Buffer
	if code := reportError(&buf, nil); code != 0 || buf.Len() != 0 {
		t.Errorf("nil: exit %d, output %q", code, buf.String())
	}
	if code := reportError(&buf, silentError{}); code != 1 || buf.Len() != 0 {
		t.Errorf("silent: exit %d, output %q", code, buf.String())
	}
	if code := reportError(&buf, io.EOF); code != 1 || buf.String() != "error: EOF\n" {
		t.Errorf("plain: exit %d, output %q", code, buf.String())
	}
	buf.Reset()
	if code := reportError(&buf, asUsage("kill", io.EOF)); code != 2 || !strings.HasPrefix(buf.String(), "error: EOF\nusage: flynn kill") {
		t.Errorf("usage: exit %d, output %q", code, buf.String())
	}
	if asUsage("kill", nil) != nil {
		t.Error("asUsage(nil) is not nil")
	}
	if ue := usageErr("kill"); !errors.Is(asUsage("steer", ue), ue) {
		t.Error("asUsage rewrapped an error that was already a usage error")
	}
	buf.Reset()
	warn(&buf, "x %d", 1)
	if buf.String() != "warning: x 1\n" {
		t.Errorf("warn wrote %q", buf.String())
	}
}

type silentError struct{}

func (silentError) Error() string { return "" }

// TestSubcommandPagesListTheirParent: a subcommand's page shows its own synopsis, and a
// group's page lists its subcommands and points at their pages.
func TestSubcommandPagesListTheirParent(t *testing.T) {
	sub := commandPage(t, "models", "fetch")
	if !strings.Contains(sub, "  flynn models fetch") {
		t.Errorf("the models fetch page has no synopsis:\n%s", sub)
	}
	group := commandPage(t, "models")
	for _, w := range []string{"Commands:", "  fetch", "Run 'flynn help models <command>'"} {
		if !strings.Contains(group, w) {
			t.Errorf("the models page does not contain %q:\n%s", w, group)
		}
	}
	flags := runCLIIn(t, t.TempDir(), "help", "flags")
	if flags.code != 0 || !strings.Contains(flags.stdout, "--goal-spec <file>") {
		t.Errorf("flynn help flags: exit %d, stdout:\n%s", flags.code, flags.stdout)
	}
}

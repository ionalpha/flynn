package main

import (
	"flag"
	"io"
	"slices"
	"strings"
	"testing"
)

const strayFlag = "--no-such-flag"

// freeText are the command paths whose words after the first positional are the
// operator's text, where a flag-looking word is content. A flag before the text is still
// refused for them.
var freeText = map[string]string{
	"goal":              "the objective may mention a flag",
	"integrations call": "the input is JSON",
	"kill":              "the reason is free text",
	"models run":        "the words after the id are the prompt",
	"playbook run":      "the input is JSON",
	"steer":             "the instruction is free text",
}

// leafPaths is every command path a handler runs, with a placeholder for each word its
// synopsis requires. A parent with subcommands is covered through them.
func leafPaths() map[string][]string {
	out := map[string][]string{}
	var walk func(prefix []string, cmds []cliCommand)
	walk = func(prefix []string, cmds []cliCommand) {
		for _, c := range cmds {
			path := append(slices.Clone(prefix), c.name)
			if len(c.subs) > 0 {
				walk(path, c.subs)
				continue
			}
			words := 0
			for _, u := range c.usage {
				required, _, _ := strings.Cut(u, "[")
				// A placeholder after a flag is that flag's value, not a word.
				words = max(words, strings.Count(required, "<")-strings.Count(required, " --"))
			}
			out[strings.Join(path, " ")] = slices.Repeat([]string{"x"}, max(words, 0))
		}
	}
	walk(nil, cliCommands())
	delete(out, "help")
	return out
}

// isolatedCLI runs the command line with every per-user directory pointed at an empty one.
func isolatedCLI(t *testing.T, args ...string) runResult {
	t.Helper()
	home := fileVaultEnv(t)
	for _, k := range []string{"APPDATA", "LOCALAPPDATA", "XDG_CONFIG_HOME", "HOME", "USERPROFILE"} {
		t.Setenv(k, home)
	}
	return runCLIIn(t, home, args...)
}

// TestAStrayFlagIsRefusedOnEitherSideOfThePositionals runs every command with a flag it
// does not have, before and after its words. Succeeding means the flag was dropped, and an
// error that does not name it means it was read as something else, such as a run id.
func TestAStrayFlagIsRefusedOnEitherSideOfThePositionals(t *testing.T) {
	for path, words := range leafPaths() {
		cmd := strings.Fields(path)
		orders := map[string][]string{"before": append([]string{strayFlag}, words...)}
		if len(words) > 0 && freeText[path] == "" {
			orders["after"] = append(slices.Clone(words), strayFlag)
		}
		for order, rest := range orders {
			t.Run(path+"/"+order, func(t *testing.T) {
				got := isolatedCLI(t, append(slices.Clone(cmd), rest...)...)
				if got.code == 0 {
					t.Fatalf("exit 0: the flag was dropped\nstdout:\n%s", got.stdout)
				}
				if !strings.Contains(got.stderr, strings.TrimLeft(strayFlag, "-")) {
					t.Fatalf("the error does not name %s:\n%s", strayFlag, got.stderr)
				}
			})
		}
	}
}

// TestEveryDocumentedFlagIsDefinedByItsHandler passes each flag on a command's help page
// to the command. The trailing stray flag stops it before it does any work.
func TestEveryDocumentedFlagIsDefinedByItsHandler(t *testing.T) {
	var walk func(prefix []string, cmds []cliCommand)
	walk = func(prefix []string, cmds []cliCommand) {
		for _, c := range cmds {
			path := append(slices.Clone(prefix), c.name)
			walk(path, c.subs)
			if len(c.subs) > 0 {
				continue
			}
			for _, f := range c.flags {
				t.Run(strings.Join(path, " ")+"/"+f.name, func(t *testing.T) {
					args := append(slices.Clone(path), "--"+f.name+"=1", strayFlag)
					got := isolatedCLI(t, args...)
					if strings.Contains(got.stderr, "not defined: -"+f.name+"\n") {
						t.Fatalf("the help page lists --%s and the command does not define it:\n%s", f.name, got.stderr)
					}
				})
			}
		}
	}
	walk(nil, cliCommands())
}

// TestAFlagBeforeTheRunIDIsReadAsAFlag covers the commands that take one run id.
func TestAFlagBeforeTheRunIDIsReadAsAFlag(t *testing.T) {
	for _, args := range [][]string{
		{"-v", "run-1"},
		{"run-1", "-v"},
		{"--verbose", "run-1"},
		{"run-1", "--verbose"},
	} {
		id, verbose, err := runIDArgs("inspect", args, false)
		if err != nil || id != "run-1" || !verbose {
			t.Errorf("runIDArgs(%q) = %q, %v, %v; want run-1, verbose", args, id, verbose, err)
		}
		got := isolatedCLI(t, append([]string{"inspect"}, args...)...)
		if !strings.Contains(got.stderr, `"run-1"`) {
			t.Errorf("inspect %q did not look up run-1:\n%s", args, got.stderr)
		}
	}
}

// TestAWordThatWouldDropALaterFlagIsRefused covers handlers that index their arguments,
// where a surplus word would leave it and any flag after it unread.
func TestAWordThatWouldDropALaterFlagIsRefused(t *testing.T) {
	for _, line := range []string{
		"upgrade junk --check",
		"version list junk",
		"db reset --dry-run",
		"db path junk",
		"models fetch one two",
		"models probe one two",
		"models bless one two",
		"models install one two",
		"spine verify --file record.json run-1",
		"spine verify one two",
		"serve junk --read-only",
		"mcp serve junk --read-only",
		"integrations call a b {} extra",
	} {
		t.Run(line, func(t *testing.T) {
			if got := isolatedCLI(t, strings.Fields(line)...); got.code == 0 {
				t.Fatalf("exit 0\nstdout:\n%s", got.stdout)
			}
		})
	}
}

// TestEveryCommandReadsOrRefusesEachRunControl crosses every command with every run
// control: one the command does not list in runFlags is a usage error, so a ceiling or a
// gate is never silently absent.
func TestEveryCommandReadsOrRefusesEachRunControl(t *testing.T) {
	controls := runControlFlags()
	if len(controls) == 0 {
		t.Fatal("no run controls: the group was renamed")
	}
	commands := cliCommands()
	for _, name := range controls {
		fs := rootFlagSet(t)
		f := fs.Lookup(name)
		if f == nil {
			t.Fatalf("no root flag %q", name)
		}
		set := flag.NewFlagSet("flynn", flag.ContinueOnError)
		set.SetOutput(io.Discard)
		set.Var(f.Value, name, "")
		if err := set.Parse([]string{"--" + name + "=1"}); err != nil {
			t.Fatalf("--%s=1: %v", name, err)
		}
		for i := range commands {
			c := &commands[i]
			if c.name == "help" {
				continue
			}
			err := refuseUnreadRunFlags(c, set)
			if reads := slices.Contains(c.runFlags, name); reads != (err == nil) {
				t.Errorf("flynn --%s %s: reads %v, refusal %v", name, c.name, reads, err)
			}
		}
	}
	if got := isolatedCLI(t, "--max-cost", "1", "db", "path"); got.code == 0 || !strings.Contains(got.stderr, "max-cost") {
		t.Errorf("the router did not refuse an unread run control: %+v", got)
	}
}

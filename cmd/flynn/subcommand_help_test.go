package main

import (
	"io"
	"os"
	"strings"
	"testing"
)

// helpPaths is every command path the command line routes, aliases and subcommands
// included, taken from the registry so a command added later is covered without anyone
// remembering to list it.
func helpPaths() [][]string {
	var paths [][]string
	for _, c := range cliCommands() {
		for _, name := range append([]string{c.name}, c.aliases...) {
			paths = append(paths, []string{name})
			for _, s := range c.subs {
				for _, sub := range append([]string{s.name}, s.aliases...) {
					paths = append(paths, []string{name, sub})
				}
			}
		}
	}
	return paths
}

// commandPage is the page `flynn help <path>` prints.
func commandPage(t *testing.T, path ...string) string {
	t.Helper()
	var page strings.Builder
	if code := showHelp(&page, io.Discard, rootFlagSet(t), path); code != 0 {
		t.Fatalf("flynn help %s exited %d", strings.Join(path, " "), code)
	}
	return page.String()
}

// TestEverySubcommandAnswersHelpWithoutDoingWork: `-h` and `--help` after a command or
// subcommand print that command's page and exit 0 before anything resolves a model, opens
// the store or looks at the network. Before, `flynn goal -h` went to the model as the
// objective and `flynn regrade -h` ran the regrade, so asking how to use a command could do
// what the command does.
func TestEverySubcommandAnswersHelpWithoutDoingWork(t *testing.T) {
	for _, path := range helpPaths() {
		for _, flagArg := range []string{"-h", "--help", "-help"} {
			t.Run(strings.Join(path, " ")+" "+flagArg, func(t *testing.T) {
				dir := t.TempDir()
				got := runCLIIn(t, dir, append(append([]string{}, path...), flagArg)...)
				if got.code != 0 {
					t.Fatalf("exit = %d, want 0\nstdout: %s\nstderr: %s", got.code, got.stdout, got.stderr)
				}
				if want := commandPage(t, path...); got.stdout != want {
					t.Fatalf("stdout is not the command's page:\n%s", got.stdout)
				}
				if got.stderr != "" {
					t.Fatalf("help reported something on stderr, so it reached for a model or the store: %s", got.stderr)
				}
				if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
					t.Fatalf("help touched the data directory: %v (err %v)", entries, err)
				}
			})
		}
	}
}

// TestHelpCommandMatchesHelpFlag: `flynn help <path>` and `flynn <path> --help` are the
// same page, so neither spelling can drift into a different answer.
func TestHelpCommandMatchesHelpFlag(t *testing.T) {
	for _, path := range [][]string{{"goal"}, {"models", "fetch"}, {"sessions"}} {
		viaCommand := runCLIIn(t, t.TempDir(), append([]string{"help"}, path...)...)
		viaFlag := runCLIIn(t, t.TempDir(), append(append([]string{}, path...), "--help")...)
		if viaCommand.code != 0 || viaCommand.stdout != viaFlag.stdout {
			t.Errorf("flynn help %s (exit %d) differs from the --help page:\n%s\n---\n%s",
				strings.Join(path, " "), viaCommand.code, viaCommand.stdout, viaFlag.stdout)
		}
	}
}

// TestHelpAfterGoalFlagsStillPrintsUsage: a help flag that follows other run flags is read
// by the flag parse rather than by the first-argument check, and must end on the same page.
func TestHelpAfterGoalFlagsStillPrintsUsage(t *testing.T) {
	got := runCLI(t, "goal", "--fanout", "-h")
	if got.code != 0 {
		t.Fatalf("exit = %d, want 0\nstderr: %s", got.code, got.stderr)
	}
	if got.stdout != commandPage(t, "goal") {
		t.Fatalf("stdout is not the goal page: %q", got.stdout)
	}
}

// TestHelpWordsInAnObjectiveStayObjective: only a lone help flag asks for help. An objective
// that mentions one is still an objective.
func TestHelpWordsInAnObjectiveStayObjective(t *testing.T) {
	if _, ok := helpTarget([]string{"goal", "document the -h flag"}); ok {
		t.Fatal("an objective containing a help flag was read as a request for help")
	}
	if _, ok := helpTarget([]string{"goal", "fix", "-h"}); ok {
		t.Fatal("a help flag after objective words was read as a request for help")
	}
	if topic, ok := helpTarget([]string{"goal", "-h", "extra"}); !ok || strings.Join(topic, " ") != "goal" {
		t.Fatalf("a help flag as the first argument gave %q, %v; want the goal page", topic, ok)
	}
	if _, ok := helpTarget([]string{"not-a-command", "-h"}); ok {
		t.Fatal("an unknown word followed by -h was read as a request for help")
	}
}

// TestAllowRefusalSaysHowItRelatesToTheGrant: an --allow that matches no --irreversible
// authorizes nothing, and the refusal has to say what --allow is for, or the operator reads
// it as the flag that widens what the run may do.
func TestAllowRefusalSaysHowItRelatesToTheGrant(t *testing.T) {
	err := checkAllowedAreMarked([]string{"deploy"}, []string{"bash"})
	if err == nil {
		t.Fatal("an --allow with no matching --irreversible was accepted")
	}
	for _, want := range []string{"--allow bash", "normal grant", "--irreversible bash"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not say %q", err, want)
		}
	}
	if err := checkAllowedAreMarked([]string{"bash"}, []string{" bash "}); err != nil {
		t.Fatalf("a matched pair was refused: %v", err)
	}
}

// TestGoalPageSaysHowAllowAndIrreversibleRelateToTheGrant: the two flags are described
// against the run's normal grant, since that is the question an operator reading them has.
// The flag rows are one line each, so the goal page carries the explanation.
func TestGoalPageSaysHowAllowAndIrreversibleRelateToTheGrant(t *testing.T) {
	page := strings.Join(strings.Fields(commandPage(t, "goal")), " ")
	for name, want := range map[string]string{
		"irreversible": "adds a check on top of the run's normal grant",
		"allow":        "grants nothing the run's normal grant",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the goal page does not say how --%s relates to the normal grant (%q)", name, want)
		}
	}
}

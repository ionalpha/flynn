package main

import (
	"flag"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
)

// helpSubcommands is every name the command line routes, taken from the routing tables so
// a subcommand added later is covered without anyone remembering to list it.
func helpSubcommands() []string {
	var names []string
	for n := range invocationCommands {
		names = append(names, n)
	}
	for n := range dataDirCommands {
		names = append(names, n)
	}
	for n := range commandAliases {
		names = append(names, n)
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// TestEverySubcommandAnswersHelpWithoutDoingWork: `-h` and `--help` after a subcommand print
// usage and exit 0 before anything resolves a model, opens the store or looks at the network.
// Before, `flynn goal -h` went to the model as the objective and `flynn regrade -h` ran the
// regrade, so asking how to use a command could do what the command does.
func TestEverySubcommandAnswersHelpWithoutDoingWork(t *testing.T) {
	for _, name := range helpSubcommands() {
		for _, flagArg := range []string{"-h", "--help", "-help"} {
			t.Run(name+" "+flagArg, func(t *testing.T) {
				dir := t.TempDir()
				got := runCLIIn(t, dir, name, flagArg)
				if got.code != 0 {
					t.Fatalf("exit = %d, want 0\nstdout: %s\nstderr: %s", got.code, got.stdout, got.stderr)
				}
				var want strings.Builder
				printUsage(&want)
				if got.stdout != want.String() {
					t.Fatalf("stdout is not the usage text: %q", got.stdout)
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

// TestHelpAfterGoalFlagsStillPrintsUsage: a help flag that follows other run flags is read
// by the flag parse rather than by the first-argument check, and must end the same way.
func TestHelpAfterGoalFlagsStillPrintsUsage(t *testing.T) {
	got := runCLI(t, "goal", "--fanout", "-h")
	if got.code != 0 {
		t.Fatalf("exit = %d, want 0\nstderr: %s", got.code, got.stderr)
	}
	var want strings.Builder
	printUsage(&want)
	if got.stdout != want.String() {
		t.Fatalf("stdout is not the usage text: %q", got.stdout)
	}
}

// TestHelpWordsInAnObjectiveStayObjective: only a lone help flag asks for help. An objective
// that mentions one is still an objective.
func TestHelpWordsInAnObjectiveStayObjective(t *testing.T) {
	if helpRequested([]string{"goal", "document the -h flag"}) {
		t.Fatal("an objective containing a help flag was read as a request for help")
	}
	if helpRequested([]string{"goal", "-h", "extra"}) != true {
		t.Fatal("a help flag as the first argument was not read as a request for help")
	}
	if helpRequested([]string{"not-a-command", "-h"}) {
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

// TestFlagHelpSaysHowAllowAndIrreversibleRelateToTheGrant: the two flags are described
// against the run's normal grant, since that is the question an operator reading them has.
func TestFlagHelpSaysHowAllowAndIrreversibleRelateToTheGrant(t *testing.T) {
	fs := flag.NewFlagSet("flynn", flag.ContinueOnError)
	var usage strings.Builder
	fs.SetOutput(&usage)
	run(fs, []string{"-h"}, io.Discard, io.Discard)
	for name, want := range map[string]string{
		"irreversible": "adds a check on top of the run's normal grant",
		"allow":        "grants nothing the run's normal grant",
	} {
		f := fs.Lookup(name)
		if f == nil || !strings.Contains(f.Usage, want) {
			t.Errorf("--%s help does not say how it relates to the normal grant (%q)", name, want)
		}
	}
}

package main

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/diag"
	"github.com/ionalpha/flynn/internal/version"
	"github.com/ionalpha/flynn/notices"
	"github.com/ionalpha/flynn/provider"
)

// runResult is what one in-process dispatch produced: the exit code and the two streams.
type runResult struct {
	code   int
	stdout string
	stderr string
}

// runCLI drives run exactly as main does, but on a flag set that hands parse errors
// back instead of exiting the process, with the streams captured and the data directory
// pointed at a fresh temporary one. The notice channel is switched off so no command
// under test talks to the network.
func runCLI(t *testing.T, args ...string) runResult {
	t.Helper()
	t.Setenv(notices.OffEnv, "1")
	// No provider credential may leak in from the developer's environment: a command that
	// resolved a real model would go on to do real work.
	for _, k := range provider.CredentialEnvVars() {
		t.Setenv(k, "")
	}
	t.Setenv(diag.EnvDir, "")

	// modelSpecExplicit is process-wide state that run writes; put it back so the order
	// tests run in cannot change what a later one sees.
	prior := modelSpecExplicit
	t.Cleanup(func() { modelSpecExplicit = prior })

	var out, errs bytes.Buffer
	fs := flag.NewFlagSet("flynn", flag.ContinueOnError)
	fs.SetOutput(&errs)
	full := append([]string{"--data-dir", t.TempDir()}, args...)
	code := run(fs, full, &out, &errs)
	return runResult{code: code, stdout: out.String(), stderr: errs.String()}
}

// TestRunVersionFlag: --version answers and stops, without opening a store or dispatching
// anything.
func TestRunVersionFlag(t *testing.T) {
	got := runCLI(t, "--version")
	if got.code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", got.code, got.stderr)
	}
	if strings.TrimSpace(got.stdout) != version.String() {
		t.Fatalf("stdout = %q, want the version %q", got.stdout, version.String())
	}
}

// TestRunHelp: `flynn help` writes the command summary to stdout and succeeds, while an
// unrecognised subcommand writes the same summary to stderr and exits 2. The stream is
// the difference: help was asked for, a typo was not.
func TestRunHelp(t *testing.T) {
	help := runCLI(t, "help")
	if help.code != 0 {
		t.Fatalf("help exit = %d, want 0", help.code)
	}
	if !strings.Contains(help.stdout, "flynn goal") {
		t.Fatalf("help did not print the usage to stdout:\n%s", help.stdout)
	}

	typo := runCLI(t, "nosuchcommand")
	if typo.code != 2 {
		t.Fatalf("unknown subcommand exit = %d, want 2", typo.code)
	}
	if !strings.Contains(typo.stderr, "flynn goal") {
		t.Fatalf("an unknown subcommand did not print the usage to stderr:\n%s", typo.stderr)
	}
	if typo.stdout != "" {
		t.Fatalf("a usage error must not write to stdout, got %q", typo.stdout)
	}
}

// TestRunUsageErrorsExitTwo: a subcommand that is missing its one required argument is a
// usage error (exit 2), told apart from a command that ran and failed (exit 1).
func TestRunUsageErrorsExitTwo(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"goal with no objective", []string{"goal"}, `usage: flynn goal`},
		{"goal with blank objective", []string{"goal", "   "}, `usage: flynn goal`},
		{"inspect with no run id", []string{"inspect"}, "usage: flynn inspect"},
		{"replay with no run id", []string{"replay"}, "usage: flynn inspect"},
		{"resume with no run id", []string{"resume"}, "usage: flynn resume"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := runCLI(t, tc.args...)
			if got.code != 2 {
				t.Fatalf("exit = %d, want 2 (stderr: %s)", got.code, got.stderr)
			}
			if !strings.Contains(got.stderr, tc.want) {
				t.Fatalf("stderr = %q, want it to contain %q", got.stderr, tc.want)
			}
		})
	}
}

// TestRunBadFlagIsAUsageError: a flag set that reports parse errors (rather than exiting
// the process, as the real one does) turns a bad flag into exit 2.
func TestRunBadFlagIsAUsageError(t *testing.T) {
	got := runCLI(t, "--no-such-flag")
	if got.code != 2 {
		t.Fatalf("exit = %d, want 2", got.code)
	}
}

// TestRunLeakWatchNeedsABundle: --leak-watch has nowhere to look and nowhere to write
// without --profile, so it is refused up front rather than running a soak that watches
// nothing.
func TestRunLeakWatchNeedsABundle(t *testing.T) {
	got := runCLI(t, "--leak-watch", "help")
	if got.code != 2 {
		t.Fatalf("exit = %d, want 2", got.code)
	}
	if !strings.Contains(got.stderr, "--leak-watch needs a bundle") {
		t.Fatalf("stderr = %q, want the --leak-watch usage message", got.stderr)
	}
}

// TestRunProfileBundleIsWritten: --profile opens a bundle for the life of the command and
// seals it on the single exit path, so the directory holds a bundle afterwards and the
// command's own exit code is unaffected by the profiler.
func TestRunProfileBundleIsWritten(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bundle")
	got := runCLI(t, "--profile", dir, "help")
	if got.code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", got.code, got.stderr)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("the profile directory was not created: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("the profile bundle is empty; nothing was sealed on the way out")
	}
}

// TestRunDataDirCommandDispatch: the table-driven subcommands share one path, so a
// success returns 0 and a failure reports the error on stderr and returns 1.
func TestRunDataDirCommandDispatch(t *testing.T) {
	ok := runCLI(t, "version")
	if ok.code != 0 {
		t.Fatalf("`flynn version` exit = %d, want 0 (stderr: %s)", ok.code, ok.stderr)
	}

	bad := runCLI(t, "extensions", "nosuchsubcommand")
	if bad.code != 1 {
		t.Fatalf("a failing subcommand exit = %d, want 1", bad.code)
	}
	if !strings.Contains(bad.stderr, "error:") || !strings.Contains(bad.stderr, "unknown subcommand") {
		t.Fatalf("stderr = %q, want the subcommand's error", bad.stderr)
	}
}

// TestRunListsRuns: `flynn runs` (and its `sessions` alias) reach the listing over an
// empty store and succeed; an empty history is not an error.
func TestRunListsRuns(t *testing.T) {
	for _, name := range []string{"runs", "sessions"} {
		got := runCLI(t, name)
		if got.code != 0 {
			t.Fatalf("`flynn %s` exit = %d, want 0 (stderr: %s)", name, got.code, got.stderr)
		}
	}
}

// TestRunInspectUnknownRun: a command that ran and failed exits 1 with its error on
// stderr, which is what distinguishes it from the usage errors above.
func TestRunInspectUnknownRun(t *testing.T) {
	got := runCLI(t, "inspect", "no-such-run")
	if got.code != 1 {
		t.Fatalf("exit = %d, want 1 (stderr: %s)", got.code, got.stderr)
	}
	if !strings.Contains(got.stderr, "error:") {
		t.Fatalf("stderr = %q, want the failure reported", got.stderr)
	}
}

// TestRunServeWithNothingConfigured: the serve branch dispatches, and a serve with no
// channel and no API is refused rather than idling forever.
func TestRunServeWithNothingConfigured(t *testing.T) {
	got := runCLI(t, "serve")
	if got.code != 1 {
		t.Fatalf("exit = %d, want 1 (stderr: %s)", got.code, got.stderr)
	}
	if !strings.Contains(got.stderr, "nothing to do") {
		t.Fatalf("stderr = %q, want the serve refusal", got.stderr)
	}
}

// TestRunGoalRefusesFanoutWithAnExternalAgent: fan-out delegates to concurrent native
// child loops, and an external harness owns its own loop, so the combination is refused
// rather than silently ignored.
func TestRunGoalRefusesFanoutWithAnExternalAgent(t *testing.T) {
	got := runCLI(t, "--model", "codex:gpt-5", "--fanout", "goal", "tidy the repo")
	if got.code != 1 {
		t.Fatalf("exit = %d, want 1 (stderr: %s)", got.code, got.stderr)
	}
	if !strings.Contains(got.stderr, "--fanout is not supported") {
		t.Fatalf("stderr = %q, want the fan-out refusal", got.stderr)
	}
}

// TestRunGoalRefusesAnUncredentialedProvider: a provider named on the command line
// carries an instruction about who may see the work, so a missing credential is refused
// rather than answered by quietly sending the goal to a different provider.
func TestRunGoalRefusesAnUncredentialedProvider(t *testing.T) {
	got := runCLI(t, "--model", "anthropic:claude-opus-4-8", "goal", "tidy the repo")
	if got.code != 1 {
		t.Fatalf("exit = %d, want 1 (stderr: %s)", got.code, got.stderr)
	}
	if !strings.Contains(got.stderr, "credential") {
		t.Fatalf("stderr = %q, want the missing credential to be the reason", got.stderr)
	}
}

// TestRunResumeRefusesAnUncredentialedProvider: a resume re-drives the run through a
// model, so it is refused for the same reason and before the run is touched.
func TestRunResumeRefusesAnUncredentialedProvider(t *testing.T) {
	got := runCLI(t, "--model", "anthropic:claude-opus-4-8", "resume", "some-run")
	if got.code != 1 {
		t.Fatalf("exit = %d, want 1 (stderr: %s)", got.code, got.stderr)
	}
	if !strings.Contains(got.stderr, "credential") {
		t.Fatalf("stderr = %q, want the missing credential to be the reason", got.stderr)
	}
}

// TestRunReviewRefusesAnExternalAgent: a verdict cannot rest on a loop whose reasoning is
// unobserved, so the review branch dispatches and refuses the backend by name.
func TestRunReviewRefusesAnExternalAgent(t *testing.T) {
	got := runCLI(t, "--model", "codex:gpt-5", "review", "owner/repo#1")
	if got.code != 1 {
		t.Fatalf("exit = %d, want 1 (stderr: %s)", got.code, got.stderr)
	}
	if !strings.Contains(got.stderr, "native loop") {
		t.Fatalf("stderr = %q, want the review refusal", got.stderr)
	}
}

// TestRunRegrade: re-grading learned skills against an empty store is a no-op that
// succeeds, so the branch dispatches without a model behind it.
func TestRunRegrade(t *testing.T) {
	got := runCLI(t, "regrade")
	if got.code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", got.code, got.stderr)
	}
}

// TestFlagPassed tells a flag the user typed apart from one left at its default, which is
// what decides whether a missing credential is refused or answered by another provider.
func TestFlagPassed(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.String("model", "default", "")
	fs.String("data-dir", "", "")
	if err := fs.Parse([]string{"--model", "openai:gpt"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !flagPassed(fs, "model") {
		t.Error("--model was passed but flagPassed says it was not")
	}
	if flagPassed(fs, "data-dir") {
		t.Error("--data-dir was left at its default but flagPassed says it was passed")
	}
}

// TestEffectiveModelSpec: an explicit --model wins; otherwise a recorded default applies;
// otherwise the flag's built-in default does.
func TestEffectiveModelSpec(t *testing.T) {
	dir := t.TempDir()
	if got := effectiveModelSpec("anthropic:a", true, dir); got != "anthropic:a" {
		t.Fatalf("explicit spec = %q, want it honoured verbatim", got)
	}
	if got := effectiveModelSpec("anthropic:a", false, dir); got != "anthropic:a" {
		t.Fatalf("with nothing recorded the flag default must apply, got %q", got)
	}
	if err := writeActiveModel(dir, "openai:saved"); err != nil {
		t.Fatalf("record a default model: %v", err)
	}
	if got := effectiveModelSpec("anthropic:a", false, dir); got != "openai:saved" {
		t.Fatalf("a recorded default must apply when --model is absent, got %q", got)
	}
	if got := effectiveModelSpec("anthropic:a", true, dir); got != "anthropic:a" {
		t.Fatalf("an explicit --model must beat the recorded default, got %q", got)
	}
}

// TestProfileConfig: the flags and the environment are merged, neither can switch off a
// watchdog the other turned on, and a watchdog with no bundle is refused.
func TestProfileConfig(t *testing.T) {
	t.Run("no profile is a zero config", func(t *testing.T) {
		t.Setenv(diag.EnvDir, "")
		cfg, usage := profileConfig("", false, false, false)
		if usage != "" {
			t.Fatalf("usage = %q, want none", usage)
		}
		if cfg.Dir != "" || cfg.Leak != nil {
			t.Fatalf("config = %+v, want an empty bundle with no watchdog", cfg)
		}
	})

	t.Run("leak-watch without a bundle is refused", func(t *testing.T) {
		t.Setenv(diag.EnvDir, "")
		cfg, usage := profileConfig("", false, true, false)
		if usage == "" {
			t.Fatal("a watchdog with no bundle must be refused")
		}
		if cfg.Dir != "" || cfg.Leak != nil {
			t.Fatalf("a refused config must be empty, got %+v", cfg)
		}
	})

	t.Run("flags configure the bundle and the watchdog", func(t *testing.T) {
		t.Setenv(diag.EnvDir, "")
		dir := t.TempDir()
		cfg, usage := profileConfig(dir, true, true, true)
		if usage != "" {
			t.Fatalf("usage = %q, want none", usage)
		}
		if cfg.Dir != dir || !cfg.Contention {
			t.Fatalf("config = %+v, want the flags honoured", cfg)
		}
		if cfg.Leak == nil || !cfg.Leak.Repeat || cfg.Leak.Logger == nil {
			t.Fatalf("watchdog = %+v, want repeat on and a logger wired", cfg.Leak)
		}
		if cfg.Children == nil {
			t.Fatal("the bundle must sample the child-process count")
		}
	})

	t.Run("the environment supplies the bundle", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv(diag.EnvDir, dir)
		cfg, usage := profileConfig("", false, true, false)
		if usage != "" {
			t.Fatalf("usage = %q, want none: the environment supplied the bundle", usage)
		}
		if cfg.Dir != dir {
			t.Fatalf("dir = %q, want the environment's %q", cfg.Dir, dir)
		}
		if cfg.Leak == nil {
			t.Fatal("--leak-watch must turn the watchdog on over an environment-supplied bundle")
		}
	})
}

// TestDefaultDataDirIsNamedForTheBuild: a development build keeps its own directory, so
// working on the schema never migrates a real installation's database.
func TestDefaultDataDirIsNamedForTheBuild(t *testing.T) {
	name := dataDirName()
	if version.IsDev() {
		if name != "flynn-dev" {
			t.Fatalf("dev build data dir = %q, want flynn-dev", name)
		}
	} else if name != "flynn" {
		t.Fatalf("release build data dir = %q, want flynn", name)
	}
	dir := defaultDataDir()
	if dir == "" {
		t.Fatal("the default data directory must never be empty")
	}
	if filepath.Base(dir) != name && dir != "."+name {
		t.Fatalf("default data dir %q does not end in the build's directory name %q", dir, name)
	}
}

// TestPrintUsageListsEverySubcommand: the summary is the only discovery surface for the
// command set, so a subcommand that dispatches must appear in it.
func TestPrintUsageListsEverySubcommand(t *testing.T) {
	var buf bytes.Buffer
	printUsage(&buf)
	out := buf.String()
	for _, want := range []string{"goal", "runs", "resume", "serve", "review", "extensions", "auth", "models", "spine"} {
		if !strings.Contains(out, "flynn "+want) {
			t.Errorf("the usage summary does not mention `flynn %s`", want)
		}
	}
}

// TestSweepsAreHousekeeping: both startup sweeps run in the background and report
// nothing, so no command depends on either having run. The sandbox cutoff must stay far
// beyond any plausible confined command: a cutoff near one would collect a live sandbox's
// profile out from under it and break the command that owns it.
func TestSweepsAreHousekeeping(t *testing.T) {
	if staleSandboxProfileAge < 12*time.Hour {
		t.Fatalf("the stale-profile cutoff is %s, close enough to a running command to collect its profile", staleSandboxProfileAge)
	}
	// Neither hands the caller anything to handle, which is what makes them safe to fire
	// and forget on every start.
	sweepStaleSandboxProfiles()
	sweepSupersededBinaries()
}

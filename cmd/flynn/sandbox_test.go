package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/sandbox"
)

func readyResult(context.Context) sandbox.Readiness {
	return sandbox.Readiness{Ready: true, Detail: "confined no-op child ran at: user-namespace", Arch: "amd64"}
}

func refusedResult(context.Context) sandbox.Readiness {
	return sandbox.Readiness{
		Cause:       sandbox.CauseAppArmorUserns,
		Also:        []sandbox.Cause{sandbox.CauseNamespaceRestricted},
		Stage:       "network-namespace",
		Detail:      "operation not permitted",
		Fix:         "install the profile",
		Alternative: "use the container tier or the microVM tier",
		Arch:        "arm64",
		Warnings:    []string{"RestrictNamespaces= is not enforced on arm64"},
	}
}

func runSandboxCommand(t *testing.T, check func(context.Context) sandbox.Readiness, args ...string) (stdout string, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	err = sandboxCommand(args, &out, &errOut, check)
	return out.String(), err
}

func TestSandboxCheckPassPrintsReadyAndExitsZero(t *testing.T) {
	out, err := runSandboxCommand(t, readyResult, "check")
	if err != nil {
		t.Fatalf("a ready host must succeed, got %v", err)
	}
	if !strings.Contains(out, "confinement ready") {
		t.Fatalf("output %q does not say ready", out)
	}
}

func TestSandboxCheckRefusalNamesCauseFixAndAlternativeAndFails(t *testing.T) {
	out, err := runSandboxCommand(t, refusedResult, "check")
	if err == nil {
		t.Fatal("a refusing host must fail the command")
	}
	if err.Error() != "" {
		t.Errorf("the report is the message; the error should be silent, got %q", err)
	}
	for _, want := range []string{
		"confinement refused: apparmor-userns-restricted",
		"network-namespace", "operation not permitted", "install the profile",
		"also possible: namespace-restricted", "container tier", "microVM tier",
		"will not run commands unconfined", "arm64", "not enforced",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not contain %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "confinement ready") {
		t.Errorf("a refusal printed as ready:\n%s", out)
	}
}

func TestSandboxCheckJSONCarriesTheReadinessAndTheSameExitStatus(t *testing.T) {
	out, err := runSandboxCommand(t, refusedResult, "check", "--json")
	if err == nil {
		t.Fatal("a refusing host must fail the command in JSON mode too")
	}
	var got sandbox.Readiness
	if jerr := json.Unmarshal([]byte(out), &got); jerr != nil {
		t.Fatalf("output is not JSON: %v\n%s", jerr, out)
	}
	if got.Ready || got.Cause != sandbox.CauseAppArmorUserns || got.Fix == "" {
		t.Fatalf("decoded %+v, want the refusal", got)
	}

	out, err = runSandboxCommand(t, readyResult, "check", "--json")
	if err != nil {
		t.Fatalf("a ready host must succeed in JSON mode, got %v", err)
	}
	if jerr := json.Unmarshal([]byte(out), &got); jerr != nil || !got.Ready {
		t.Fatalf("decoded %+v (%v), want ready", got, jerr)
	}
}

// TestSandboxCheckHasNoWayToAcceptARefusal tries each plausible off switch, as a flag and
// as an environment variable, and shows none turns a refusal into success or skips the
// check.
func TestSandboxCheckHasNoWayToAcceptARefusal(t *testing.T) {
	for _, arg := range []string{"--skip", "--force", "--ignore", "--allow-unconfined", "--no-check", "--insecure", "--unconfined", "--ready", "-f", "--json=false", "--help"} {
		out, err := runSandboxCommand(t, refusedResult, "check", arg)
		if err == nil {
			t.Errorf("%s: the command succeeded against a refusing host: %s", arg, out)
		}
		if strings.Contains(out, "confinement ready") {
			t.Errorf("%s printed a ready result for a refusing host", arg)
		}
	}

	for _, name := range []string{"FLYNN_SANDBOX_CONFINE", "FLYNN_SANDBOX", "FLYNN_SANDBOX_SKIP", "FLYNN_NO_SANDBOX", "FLYNN_UNCONFINED", "FLYNN_INSECURE", "CI"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, "1")
			_, err := runSandboxCommand(t, refusedResult, "check")
			if err == nil {
				t.Fatalf("%s=1 made a refusing host succeed", name)
			}
		})
	}
}

func TestSandboxCommandRejectsUsageErrors(t *testing.T) {
	for _, args := range [][]string{nil, {}, {"status"}, {"check", "extra"}, {"--json"}, {"check", "--nonsense"}} {
		called := false
		check := func(ctx context.Context) sandbox.Readiness { called = true; return readyResult(ctx) }
		out, err := runSandboxCommand(t, check, args...)
		if err == nil {
			t.Errorf("args %q succeeded: %s", args, out)
		}
		if called {
			t.Errorf("args %q ran the check on a usage error", args)
		}
	}
}

func TestSandboxCommandIsRegistered(t *testing.T) {
	if _, ok := dataDirCommands["sandbox"]; !ok {
		t.Fatal("flynn sandbox is not registered")
	}
	var usage bytes.Buffer
	printUsage(&usage)
	if !strings.Contains(usage.String(), "flynn sandbox check") {
		t.Fatal("the usage text does not list flynn sandbox check")
	}
}

// stateDir returns a directory outside the scratch areas the sandbox leaves writable, where
// a readiness file may be recorded. The default test directory is under /tmp on POSIX
// hosts, which the command rightly refuses, so those hosts use the home directory.
func stateDir(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		return t.TempDir()
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory outside the scratch areas: %v", err)
	}
	dir, err := os.MkdirTemp(home, ".flynn-ready-test-")
	if err != nil {
		t.Skipf("cannot create a directory outside the scratch areas: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestSandboxCheckReadyFileRecordsPassAndRefusal(t *testing.T) {
	for name, check := range map[string]func(context.Context) sandbox.Readiness{"pass": readyResult, "refusal": refusedResult} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(stateDir(t), "ready.json")
			_, err := runSandboxCommand(t, check, "check", "--ready-file", path)
			if (err == nil) != (name == "pass") {
				t.Fatalf("err = %v", err)
			}
			b, rerr := os.ReadFile(path)
			if rerr != nil {
				t.Fatalf("the result was not recorded: %v", rerr)
			}
			var got sandbox.Readiness
			if jerr := json.Unmarshal(b, &got); jerr != nil || got.Ready != (name == "pass") {
				t.Fatalf("recorded %s, want ready=%v (%v)", b, name == "pass", jerr)
			}
		})
	}
}

func TestSandboxCheckReadyFileInsideTheScratchAreaFailsTheCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the scratch areas are POSIX paths")
	}
	_, err := runSandboxCommand(t, readyResult, "check", "--ready-file", "/tmp/flynn-ready-test.json")
	if !errors.Is(err, sandbox.ErrReadinessFileWritable) {
		t.Fatalf("err = %v, want a refusal to record where the sandbox can write", err)
	}
}

// failingWriter refuses every write, as a closed pipe would.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("write refused") }

// TestSandboxCheckFailsWhenTheResultCannotBePrinted shows a result nobody could read is not
// reported as a success, in either output form.
func TestSandboxCheckFailsWhenTheResultCannotBePrinted(t *testing.T) {
	for _, args := range [][]string{{"check"}, {"check", "--json"}} {
		err := sandboxCommand(args, failingWriter{}, &bytes.Buffer{}, readyResult)
		if err == nil || !strings.Contains(err.Error(), "write refused") {
			t.Errorf("%v: err = %v, want the write failure", args, err)
		}
	}
}

// TestRunSandboxRejectsUsageErrors reaches the real entry point without starting a check:
// sandboxCommand refuses a missing subcommand before it runs anything.
func TestRunSandboxRejectsUsageErrors(t *testing.T) {
	if err := runSandbox(nil, ""); err == nil {
		t.Fatal("a missing subcommand must be a usage error")
	}
}

package main

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/diag"
	"github.com/ionalpha/flynn/notices"
	"github.com/ionalpha/flynn/provider"
)

// unopenableDataDir returns a data directory path no store can be opened under, because a
// regular file sits where a directory would have to be. Creating the directory fails on
// every platform, which is what makes the commands below report a failure rather than
// quietly work against a store somewhere else.
func unopenableDataDir(t *testing.T) string {
	t.Helper()
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	return filepath.Join(blocker, "data")
}

// runCLIIn drives run the way runCLI does, but against a data directory the caller chooses,
// so the failure half of each dispatch can be reached. A command that cannot open its store
// has to say so and exit non-zero: exiting 0 on a store it never opened would report an
// empty history as though it were the truth.
func runCLIIn(t *testing.T, dataDir string, args ...string) runResult {
	t.Helper()
	t.Setenv(notices.OffEnv, "1")
	for _, k := range provider.CredentialEnvVars() {
		t.Setenv(k, "")
	}
	t.Setenv(diag.EnvDir, "")

	prior := modelSpecExplicit
	t.Cleanup(func() { modelSpecExplicit = prior })

	var out, errs bytes.Buffer
	fs := flag.NewFlagSet("flynn", flag.ContinueOnError)
	fs.SetOutput(&errs)
	full := append([]string{"--data-dir", dataDir}, args...)
	code := run(fs, full, &out, &errs)
	return runResult{code: code, stdout: out.String(), stderr: errs.String()}
}

// TestRunReportsAStoreItCannotOpen walks the dispatches that read the store and checks each
// one fails loudly. The distinction being pinned is between "there is nothing recorded" and
// "the recording could not be read": the first is exit 0 and an empty listing, the second has
// to be exit 1 with the reason on stderr, or a broken data directory reads as a clean history.
func TestRunReportsAStoreItCannotOpen(t *testing.T) {
	for _, cmd := range [][]string{
		{"runs"},
		{"sessions"},
		{"regrade"},
		{"inspect", "some-run"},
		{"get", "instances"},
		{"describe", "instances", "one"},
		{"diff", "instances", "one"},
		{"watch"},
		{"deps", "check"},
		{"integrations", "show", "httpbin"},
		{"integrations", "call", "httpbin", "get"},
		{"services", "rm", "svc"},
		{"playbook", "run", "fly-app"},
	} {
		t.Run(strings.Join(cmd, " "), func(t *testing.T) {
			got := runCLIIn(t, unopenableDataDir(t), cmd...)
			if got.code != 1 {
				t.Fatalf("exit = %d, want 1 (stdout %q, stderr %q)", got.code, got.stdout, got.stderr)
			}
			if !strings.Contains(got.stderr, "error:") {
				t.Fatalf("stderr = %q, want the failure reported", got.stderr)
			}
		})
	}
}

package main

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/storage/sqlite"
)

// newerStoreDir returns a data directory whose store a newer build has opened: it holds a
// migration this build does not carry.
func newerStoreDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := dataStoreFile(dir)
	s, err := sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	if _, err := raw.Exec(`INSERT INTO schema_migrations(version, name, checksum, applied_at) VALUES (9999, '9999_from_the_future.sql', 'x', '2099-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestGoalOnAStoreFromANewerBuildExitsNonZero is what an older binary does to a data
// directory a newer one has opened. The goal must fail, say why in terms the operator can
// act on, and exit non-zero so a script or an agent driving the CLI sees the failure
// rather than reading a message and a success code.
func TestGoalOnAStoreFromANewerBuildExitsNonZero(t *testing.T) {
	// A credential that resolves, so the run gets as far as the store.
	t.Setenv("OPENAI_API_KEY", "test-key")
	var out, errOut strings.Builder
	code := routeCommand("goal", []string{"goal", "tidy the repo"}, invocation{
		stdout:    &out,
		stderr:    &errOut,
		modelSpec: "openai:gpt-test",
		dataDir:   newerStoreDir(t),
	})
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (stdout %q, stderr %q)", code, out.String(), errOut.String())
	}
	msg := errOut.String()
	if !strings.Contains(msg, "newer Flynn") || !strings.Contains(msg, "flynn upgrade") {
		t.Fatalf("stderr = %q, want a plain statement that the data is from a newer version and how to proceed", msg)
	}
	if strings.Contains(msg, "no such column") {
		t.Fatalf("stderr = %q leaks the raw SQL failure", msg)
	}
	if strings.Contains(msg, "db reset") {
		t.Fatalf("stderr = %q offers a reset, which would destroy the newer build's data", msg)
	}
}

// TestStoreReadingCommandsOnANewerStoreExitNonZero: the refusal comes from opening the
// store, so every command that reads it gets it, not just goal.
func TestStoreReadingCommandsOnANewerStoreExitNonZero(t *testing.T) {
	dir := newerStoreDir(t)
	for _, cmd := range []string{"runs", "regrade"} {
		t.Run(cmd, func(t *testing.T) {
			got := runCLIIn(t, dir, cmd)
			if got.code != 1 {
				t.Fatalf("exit = %d, want 1 (stdout %q, stderr %q)", got.code, got.stdout, got.stderr)
			}
			if !strings.Contains(got.stderr, "newer Flynn") {
				t.Fatalf("stderr = %q, want the newer-version refusal", got.stderr)
			}
		})
	}
}

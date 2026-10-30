package sqlite_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/storage/sqlite"
)

// stampFutureMigration records a migration no build carries yet, as a newer Flynn
// would after adding one, and returns the version it used.
func stampFutureMigration(t *testing.T, path string) int {
	t.Helper()
	const future = 9999
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	if _, err := raw.Exec(`INSERT INTO schema_migrations(version, name, checksum, applied_at) VALUES (?, ?, 'x', '2099-01-01T00:00:00Z')`,
		future, "9999_from_the_future.sql"); err != nil {
		t.Fatal(err)
	}
	return future
}

// TestOpenRefusesAStoreFromANewerBuild is the upgrade hazard behind "no such column":
// migration 0028 renamed skills.uses, and a build from before it opened a store that
// had it applied and failed on the first query. The refusal has to come at Open, typed,
// and say which way to go.
func TestOpenRefusesAStoreFromANewerBuild(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "flynn.db")

	s, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	future := stampFutureMigration(t, path)

	_, err = sqlite.Open(ctx, path)
	var newer *sqlite.NewerSchemaError
	if !errors.As(err, &newer) {
		t.Fatalf("Open on a store stamped with a future migration = %v, want *NewerSchemaError", err)
	}
	if newer.Applied != future || newer.Known >= future || newer.Known == 0 {
		t.Fatalf("error = %+v, want Applied %d above a nonzero Known", newer, future)
	}
	msg := err.Error()
	for _, want := range []string{"newer Flynn", "flynn upgrade", "--data-dir", "untouched"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not say %q", msg, want)
		}
	}
	if strings.Contains(msg, "reset") {
		t.Errorf("message %q offers a reset, which would destroy the newer build's data", msg)
	}
}

// TestOpenAcceptsAStoreAtThisBuildsVersion: the refusal is for a store ahead of the
// build, so a store this build migrated itself, and a fresh one, still open.
func TestOpenAcceptsAStoreAtThisBuildsVersion(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "flynn.db")
	for range 2 {
		s, err := sqlite.Open(ctx, path)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

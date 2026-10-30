package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
)

// NewerSchemaError reports that the database has migrations applied which this
// build does not carry: a newer Flynn has opened it. The migration runner only
// checks that the migrations it knows are intact, so without this refusal an older
// build opens the store and runs its own queries against columns a later migration
// renamed or dropped, and fails on the first query with a raw SQL error.
//
// The data is intact and belongs to the newer build, so the remedy is to upgrade or to
// use another data directory, never to reset the database; callers must not route this
// to the recovery that offers a reset.
type NewerSchemaError struct {
	// Applied is the highest migration version recorded in the database, and Name its file.
	Applied int
	Name    string
	// Known is the highest migration version this build carries.
	Known int
}

func (e *NewerSchemaError) Error() string {
	return fmt.Sprintf("this data directory was written by a newer Flynn (its storage schema is at version %d, %s) and this build only understands up to version %d; "+
		"upgrade Flynn with `flynn upgrade`, or point this build at a different directory with --data-dir. The data is untouched",
		e.Applied, e.Name, e.Known)
}

// refuseNewerSchema returns a [*NewerSchemaError] when db records a migration above
// the highest one embedded in this build.
//
// It runs after the migrations have been applied rather than before. An older build
// has nothing left to apply to a newer store, so the run changes nothing, and checking
// afterwards means a brand new database is never read for a version it cannot yet have.
func refuseNewerSchema(ctx context.Context, db *sql.DB) error {
	known, err := highestEmbeddedMigration()
	if err != nil {
		return err
	}
	var (
		applied int
		name    string
	)
	err = db.QueryRowContext(ctx, `SELECT version, name FROM schema_migrations ORDER BY version DESC LIMIT 1`).Scan(&applied, &name)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("sqlite: reading schema version: %w", err)
	case applied > known:
		return &NewerSchemaError{Applied: applied, Name: name, Known: known}
	}
	return nil
}

// highestEmbeddedMigration is the largest NNNN prefix among the embedded migration
// files, the same number the migration runner records as the version.
func highestEmbeddedMigration() (int, error) {
	// The pattern is a constant, so Glob cannot report a malformed one.
	names, _ := fs.Glob(migrations, "migrations/*.sql")
	return highestVersion(names)
}

// highestVersion is the largest numeric prefix among migration file names.
func highestVersion(names []string) (int, error) {
	highest := 0
	for _, n := range names {
		base := strings.TrimPrefix(n, "migrations/")
		prefix, _, _ := strings.Cut(base, "_")
		v, err := strconv.Atoi(prefix)
		if err != nil {
			return 0, fmt.Errorf("sqlite: migration %q has no numeric version: %w", n, err)
		}
		highest = max(highest, v)
	}
	return highest, nil
}

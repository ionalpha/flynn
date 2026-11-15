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

// NewerSchemaError reports a database a newer Flynn has migrated. The runner
// checks only the migrations it knows, so an older build would otherwise fail
// later with a raw SQL error on a renamed column.
//
// The data is intact: upgrade or use another data directory. Callers must not
// route this to a recovery that offers a reset.
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

// refuseNewerSchema returns a [*NewerSchemaError] when db records a migration
// above this build's highest. It runs after migrating, so a brand new database
// is never read for a version it cannot have; an older build has nothing to
// apply to a newer store.
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

func highestEmbeddedMigration() (int, error) {
	// The pattern is a constant, so Glob cannot report a malformed one.
	names, _ := fs.Glob(migrations, "migrations/*.sql")
	return highestVersion(names)
}

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

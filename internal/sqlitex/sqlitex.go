// Package sqlitex is the shared SQLite engine for the agent's durable backends.
// It owns the single way the agent opens a database (pure-Go modernc.org/sqlite,
// no cgo), applies pragmas, caps the connection pool, runs embedded migrations,
// and runs a transaction. The state and spine SQLite adapters build on it, so the
// open/pragma/tx/time boilerplate lives in exactly one place instead of being
// copied per package.
package sqlitex

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"time"

	_ "modernc.org/sqlite" // registers the pure-Go "sqlite" driver

	"github.com/ionalpha/flynn/migrate"
)

// Open opens (creating if needed) a SQLite database at dsn, applies the standard
// pragmas, caps the pool at a single connection, and migrates to the latest
// schema using the .sql files under "migrations" in migrationsFS. dsn is a file
// path, or ":memory:" for an ephemeral store.
//
// One connection: SQLite serialises writers anyway, and a single connection keeps
// a ":memory:" database alive with a consistent view. busy_timeout waits rather
// than failing on a lock; foreign_keys enforces referential integrity (a no-op
// for schemas that declare none).
func Open(ctx context.Context, dsn string, migrationsFS fs.FS) (*sql.DB, error) {
	conn := dsn + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", conn)
	if err != nil {
		return nil, fmt.Errorf("sqlitex: open: %w", err)
	}
	db.SetMaxOpenConns(1)

	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlitex: migrations fs: %w", err)
	}
	if err := migrate.Run(ctx, db, sub); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlitex: migrate: %w", err)
	}
	return db, nil
}

// Tx runs fn inside a transaction, committing on success and rolling back on any
// error, so a failed multi-statement write leaves the database unchanged.
func Tx(ctx context.Context, db *sql.DB, fn func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// FormatTime renders t as UTC RFC3339Nano, the canonical on-disk time format
// shared by every durable backend.
func FormatTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// ParseTime parses a FormatTime string back to a UTC time, returning the zero
// time if s is not valid RFC3339Nano.
func ParseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

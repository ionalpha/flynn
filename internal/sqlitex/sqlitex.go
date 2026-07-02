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

	"github.com/ionalpha/flynn/internal/migrate"
)

// Open opens (creating if needed) a SQLite database at dsn, applies the standard
// pragmas, caps the pool at a single connection, and migrates to the latest
// schema using the .sql files under "migrations" in migrationsFS. dsn is a file
// path, or ":memory:" for an ephemeral store.
//
// Pragmas, chosen for an append-heavy event-sourcing workload that must be light
// on SSDs:
//   - journal_mode(WAL): writes append to a write-ahead log instead of rewriting
//     pages through a rollback journal, which cuts write amplification and lets
//     reads run without blocking the writer. (A no-op for ":memory:".)
//   - synchronous(NORMAL): the WAL is fsynced at checkpoints rather than on every
//     commit, the configuration SQLite recommends with WAL. It is crash-safe (no
//     corruption); only the last few unsynced transactions can be lost on a power
//     cut, an acceptable trade for a local agent and far fewer fsyncs / less wear.
//   - busy_timeout(5000): wait rather than fail on a lock.
//   - foreign_keys(1): enforce referential integrity (a no-op where none declared).
//
// One connection: SQLite serialises writers anyway, and a single connection keeps
// a ":memory:" database alive with a consistent view. Reads need not queue behind
// it: OpenReadPool opens the read side of a file database.
func Open(ctx context.Context, dsn string, migrationsFS fs.FS) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dsn+dsnPragmas)
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

// dsnPragmas is the shared per-connection configuration. Beyond the write-side
// journal pragmas documented on Open:
//   - cache_size(-8000): an 8 MiB page cache per connection (the default is 2 MiB),
//     sized for a working set of projections and recent events.
//   - temp_store(2): temporary tables and sort spills stay in memory instead of
//     hitting the filesystem.
//
// mmap_size is deliberately absent: the pure-Go driver reads through its own VFS,
// where the pragma buys nothing.
const dsnPragmas = "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)" +
	"&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)" +
	"&_pragma=cache_size(-8000)&_pragma=temp_store(2)"

// OpenReadPool opens the read side of an already-open-and-migrated file database:
// a pool of maxConns connections pinned read-only with query_only, so point reads
// and sweeps run concurrently with each other and with the single writer (WAL
// gives one writer plus N readers; a lone connection forgoes the N). It returns
// (nil, nil) for ":memory:", which lives entirely inside its one write connection
// and cannot be opened twice; callers fall back to the write handle.
func OpenReadPool(dsn string, maxConns int) (*sql.DB, error) {
	if dsn == ":memory:" {
		return nil, nil
	}
	db, err := sql.Open("sqlite", dsn+dsnPragmas+"&_pragma=query_only(1)")
	if err != nil {
		return nil, fmt.Errorf("sqlitex: open read pool: %w", err)
	}
	if maxConns < 1 {
		maxConns = 4
	}
	db.SetMaxOpenConns(maxConns)
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

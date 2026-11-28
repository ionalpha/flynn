// Package sqlitex is the one way the agent opens a SQLite database (pure-Go
// modernc.org/sqlite, no cgo): pragmas, a capped pool, embedded migrations,
// transactions and the on-disk time format. The state and spine adapters build on it.
package sqlitex

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"modernc.org/sqlite" // registers the pure-Go "sqlite" driver
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/internal/fsatomic"
	"github.com/ionalpha/flynn/internal/migrate"
)

// Open opens (creating if needed) a SQLite database at dsn, a file path or ":memory:",
// applies the standard pragmas, caps the pool at one connection, and migrates with the
// .sql files under "migrations" in migrationsFS.
//
// Pragmas, chosen for an append-heavy event-sourcing workload that must be light on SSDs:
//   - journal_mode(WAL): writes append to a log instead of rewriting pages through a
//     rollback journal, cutting write amplification and letting reads run without blocking
//     the writer. (A no-op for ":memory:".)
//   - synchronous(NORMAL): the WAL is fsynced at checkpoints, not every commit. It is
//     crash-safe; only the last few unsynced transactions can be lost on a power cut.
//   - busy_timeout(5000) waits on a lock (see [WriteTxLock]); foreign_keys(1) is on.
//
// One connection, because SQLite serialises writers anyway and one connection keeps a
// ":memory:" database alive with a consistent view. OpenReadPool opens the read side.
func Open(ctx context.Context, dsn string, migrationsFS fs.FS) (*sql.DB, error) {
	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("sqlitex: migrations fs: %w", err)
	}
	return open(ctx, dsn, sub)
}

// open is [Open] over the migrations directory itself. A database that does not exist
// yet starts as a copy of the migrated image of its set ([migratedImage]), so the
// migrations replay once per process instead of once per open.
func open(ctx context.Context, dsn string, migrations fs.FS) (*sql.DB, error) {
	memory := dsn == ":memory:"
	key := imageKey(dsn, migrations)
	var img []byte
	if key != "" {
		img = cachedImage(key)
		if !memory {
			if img == nil {
				img = migratedImage(ctx, migrations, key)
			}
			if img != nil {
				// Losing the race to create the file, or a filesystem without hard
				// links, leaves SQLite to create it and the migrations to run.
				_ = fsatomic.CreateFile(dsn, img, 0o600)
			}
		}
	}

	db, err := sql.Open("sqlite", dsn+dsnPragmas+WriteTxLock)
	if err != nil {
		return nil, fmt.Errorf("sqlitex: open: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := connect(ctx, db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlitex: open: %w", err)
	}
	if memory && img != nil {
		if err := loadImage(ctx, db, img); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("sqlitex: load migrated image: %w", err)
		}
	}
	if err := migrate.Run(ctx, db, migrations); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlitex: migrate: %w", err)
	}
	if memory && key != "" && img == nil {
		storeImage(ctx, db, key)
	}
	return db, nil
}

// connectAttempts and connectStep bound how long [Open] keeps retrying its first
// connection: the wait before attempt n is n*connectStep up to connectMaxWait, about
// five seconds in all.
const (
	connectAttempts = 60
	connectStep     = 10 * time.Millisecond
	connectMaxWait  = 100 * time.Millisecond
)

// connect makes the first connection, retrying while another process holds the file.
// The first connection on a new file switches it to WAL, which needs the file to
// itself; against a concurrent switch, or a process using the old journal mode, SQLite
// returns SQLITE_BUSY at once without consulting busy_timeout. Two processes starting
// on a fresh data directory both get here, so the loser retries until the file is WAL
// and its own switch is a no-op.
func connect(ctx context.Context, db *sql.DB) error {
	var clk clock.Timing = clock.System{}
	for attempt := 1; ; attempt++ {
		err := db.PingContext(ctx)
		if err == nil || !isBusy(err) || attempt >= connectAttempts {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-clk.After(min(time.Duration(attempt)*connectStep, connectMaxWait)):
		}
	}
}

// isBusy reports whether err is SQLite's lock contention (SQLITE_BUSY or
// SQLITE_LOCKED). The primary code is the low byte; an extended code such as
// SQLITE_BUSY_SNAPSHOT is still contention.
func isBusy(err error) bool {
	var se *sqlite.Error
	if !errors.As(err, &se) {
		return false
	}
	code := se.Code() & 0xff
	return code == sqlite3.SQLITE_BUSY || code == sqlite3.SQLITE_LOCKED
}

// dsnPragmas is the per-connection configuration. Beyond the journal pragmas on Open:
//   - cache_size(-8000): an 8 MiB page cache per connection (the default is 2 MiB),
//     sized for a working set of projections and recent events.
//   - temp_store(2): temporary tables and sort spills stay in memory.
//
// mmap_size is omitted: the pure-Go driver reads through its own VFS, where the pragma
// buys nothing.
const dsnPragmas = "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)" +
	"&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)" +
	"&_pragma=cache_size(" + cacheSize + ")&_pragma=temp_store(2)"

// cacheSize is the page cache in the form PRAGMA cache_size takes: negative is KiB.
const cacheSize = "-8000"

// WriteTxLock makes every transaction on a write handle take the write lock at BEGIN
// (BEGIN IMMEDIATE). A deferred transaction holds only a read snapshot until its first
// write, and if another process committed since, SQLite refuses the upgrade with
// SQLITE_BUSY at once, without consulting busy_timeout. Two flynn processes on one
// data directory hit this (the bundled skill seed reads, then writes); taken at BEGIN,
// the lock is waited for. The read pool omits it because it never writes.
const WriteTxLock = "&_txlock=immediate"

// OpenReadPool opens the read side of an open, migrated file database: maxConns
// connections pinned read-only with query_only, so reads run concurrently with each
// other and with the single writer. It returns (nil, nil) for ":memory:", which lives
// inside its one write connection; callers fall back to the write handle.
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

// Tx runs fn in a transaction, committing on success and rolling back on any error, so
// a failed multi-statement write leaves the database unchanged.
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

// TimeLayout is the on-disk time format of every durable backend: RFC 3339 with the
// fractional second zero-padded to nine digits, so a stored value is always 30
// characters and string order is time order. time.RFC3339Nano strips trailing zeros,
// and since '.' (0x2E) sorts before 'Z' (0x5A), "2026-01-01T00:00:00Z" would sort
// after the reading one nanosecond later. About one reading in ten lands on the
// second, more on a coarse clock, so ORDER BY over unpadded columns was wrong for a
// share of rows. Migration 0029 rewrote the columns written before this layout.
const TimeLayout = "2006-01-02T15:04:05.000000000Z07:00"

// FormatTime renders t as UTC in TimeLayout.
func FormatTime(t time.Time) string { return t.UTC().Format(TimeLayout) }

// ParseTime parses a FormatTime string back to a UTC time, or the zero time if s is
// not valid RFC 3339. It reads the pre-0029 unpadded encoding too, since RFC3339Nano
// accepts any number of fractional digits.
func ParseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

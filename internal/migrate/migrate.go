// Package migrate applies versioned SQL migrations to a SQL database.
//
// Migrations are files named NNNN_description.sql, the leading integer the version.
// Each is applied once, in ascending order, in its own transaction, and recorded in
// schema_migrations. A run never corrupts a store:
//
//   - Each migration runs in a transaction, so a failure leaves no partial state
//     (SQLite and Postgres both roll back DDL).
//   - The first failure stops the run at the last good version.
//   - There are no down-migrations to run destructively in production.
//   - A SHA-256 of every applied migration is stored, so an edited applied file
//     stops the next run.
//   - A new migration whose version sorts below the latest applied is rejected
//     instead of skipped.
//
// Each migration is applied under SQLite's write lock after a re-check of
// schema_migrations, so processes opening one database at once take turns and the
// later ones skip what the first applied. The version primary key backs this up
// against a double record.
package migrate

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"
)

// IncompatibleSchemaError reports a database created by a build with different
// migrations: an applied migration's bytes changed, or a new one sorts below one
// applied. Migrations are immutable, so the database must be recreated or another
// data directory used. A distinct type lets a caller guide the user to recovery.
type IncompatibleSchemaError struct {
	Migration string // the offending migration file
	Reason    string // what is wrong with it
}

func (e *IncompatibleSchemaError) Error() string {
	return fmt.Sprintf("migrate: %s %s - migrations are immutable", e.Migration, e.Reason)
}

// Run applies every migration in fsys not yet recorded, in ascending version order,
// after verifying those already applied. A re-run on an up-to-date database does
// nothing.
func Run(ctx context.Context, db *sql.DB, fsys fs.FS) error {
	migs, err := load(fsys)
	if err != nil {
		return err
	}
	if err := ensureTable(ctx, db); err != nil {
		return err
	}
	applied, err := readApplied(ctx, db)
	if err != nil {
		return err
	}

	maxApplied := 0
	for v := range applied {
		if v > maxApplied {
			maxApplied = v
		}
	}
	// Integrity is checked before anything is applied.
	for _, m := range migs {
		if sum, ok := applied[m.version]; ok {
			if sum != m.checksum {
				return &IncompatibleSchemaError{Migration: m.name, Reason: "changed after it was applied (checksum mismatch)"}
			}
			continue
		}
		if m.version < maxApplied {
			return &IncompatibleSchemaError{Migration: m.name, Reason: fmt.Sprintf("is out of order (version %d is below the latest applied %d)", m.version, maxApplied)}
		}
	}

	for _, m := range migs {
		if _, ok := applied[m.version]; ok {
			continue
		}
		if err := apply(ctx, db, m); err != nil {
			return fmt.Errorf("migrate: applying %s: %w", m.name, err)
		}
	}
	return nil
}

type migration struct {
	version  int
	name     string
	sql      string
	checksum string
}

func load(fsys fs.FS) ([]migration, error) {
	names, err := fs.Glob(fsys, "*.sql")
	if err != nil {
		return nil, err
	}
	out := make([]migration, 0, len(names))
	for _, name := range names {
		v, err := parseVersion(name)
		if err != nil {
			return nil, fmt.Errorf("migrate: bad migration name %q: %w", name, err)
		}
		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(body)
		out = append(out, migration{
			version:  v,
			name:     name,
			sql:      string(body),
			checksum: hex.EncodeToString(sum[:]),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	for i := 1; i < len(out); i++ {
		if out[i].version == out[i-1].version {
			return nil, fmt.Errorf("migrate: duplicate version %d (%s and %s)", out[i].version, out[i-1].name, out[i].name)
		}
	}
	return out, nil
}

// parseVersion reads a filename's leading integer ("0003_add.sql" is 3).
func parseVersion(name string) (int, error) {
	base := strings.TrimSuffix(name, ".sql")
	if i := strings.IndexByte(base, '_'); i >= 0 {
		base = base[:i]
	}
	return strconv.Atoi(base)
}

func ensureTable(ctx context.Context, db *sql.DB) error {
	const ddl = `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		name       TEXT NOT NULL,
		checksum   TEXT NOT NULL,
		applied_at TEXT NOT NULL
	)`
	if _, err := db.ExecContext(ctx, ddl); err != nil {
		return fmt.Errorf("migrate: ensuring schema_migrations: %w", err)
	}
	return nil
}

func readApplied(ctx context.Context, db *sql.DB) (map[int]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT version, checksum FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("migrate: reading schema_migrations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	applied := make(map[int]string)
	for rows.Next() {
		var (
			v   int
			sum string
		)
		if err := rows.Scan(&v, &sum); err != nil {
			return nil, err
		}
		applied[v] = sum
	}
	return applied, rows.Err()
}

// apply runs m in a write-locking transaction and records it, unless the version is
// already recorded.
//
// Run picked the pending set from an unlocked read, so another process may have
// applied m since, and reapplying a non-idempotent migration (a column drop) fails.
// BEGIN IMMEDIATE takes the write lock first, so the loser waits out the busy
// timeout and then sees the winner's row; SQLite refuses a deferred transaction's
// read-to-write upgrade at once when another writer committed in between. A recorded
// version is still held to its checksum. The statement is raw on a pinned
// connection because database/sql cannot request an immediate transaction per call.
func apply(ctx context.Context, db *sql.DB, m migration) (err error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			// The caller's context may be what failed, and the lock must still be
			// released.
			_, _ = conn.ExecContext(context.WithoutCancel(ctx), `ROLLBACK`)
		}
	}()

	var sum string
	switch err := conn.QueryRowContext(ctx, `SELECT checksum FROM schema_migrations WHERE version = ?`, m.version).Scan(&sum); {
	case err == nil:
		if sum != m.checksum {
			return &IncompatibleSchemaError{Migration: m.name, Reason: "changed after it was applied (checksum mismatch)"}
		}
		return nil
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}

	if _, err := conn.ExecContext(ctx, m.sql); err != nil {
		return err
	}
	if _, err := conn.ExecContext(
		ctx,
		`INSERT INTO schema_migrations(version, name, checksum, applied_at) VALUES (?, ?, ?, ?)`,
		m.version, m.name, m.checksum, time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return err
	}
	committed = true
	return nil
}

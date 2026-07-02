// Package migrate applies versioned SQL migrations to a SQL database, robustly.
//
// Migrations are SQL files named NNNN_description.sql (the leading integer is
// the version). Each is applied once, in ascending order, in its own
// transaction, and recorded in a schema_migrations table. The design refuses to
// corrupt a store:
//
//   - Atomic: each migration runs in a transaction, so a failure leaves zero
//     partial state (SQLite and Postgres both roll back DDL).
//   - Fail-closed: the first failure stops the run at the last good version.
//   - Forward-only: there are no down-migrations to run destructively in prod.
//   - Drift-detected: a SHA-256 of every applied migration is stored; if an
//     already-applied file is later edited, the next run refuses to proceed.
//   - Order-checked: a new migration whose version sits below the latest applied
//     one (an out-of-order insertion) is rejected rather than silently skipped.
//
// The schema_migrations table is portable, so the same runner serves SQLite and
// (later) Postgres. Concurrency is guarded by the version primary key: two
// racing runs cannot both record the same migration; the loser fails safely.
package migrate

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Run applies every migration in fsys not yet recorded in schema_migrations, in
// ascending version order, after verifying the integrity of those already
// applied. It is idempotent: once up to date, a re-run does nothing.
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
	// Integrity checks before applying anything.
	for _, m := range migs {
		if sum, ok := applied[m.version]; ok {
			if sum != m.checksum {
				return fmt.Errorf("migrate: %s changed after it was applied (checksum mismatch) — migrations are immutable", m.name)
			}
			continue
		}
		if m.version < maxApplied {
			return fmt.Errorf("migrate: %s is out of order (version %d is below the latest applied %d)", m.name, m.version, maxApplied)
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

// parseVersion reads the leading integer of a filename, e.g. "0003_add.sql" -> 3.
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

func apply(ctx context.Context, db *sql.DB, m migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // no-op once committed
	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations(version, name, checksum, applied_at) VALUES (?, ?, ?, ?)`,
		m.version, m.name, m.checksum, time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		return err
	}
	return tx.Commit()
}

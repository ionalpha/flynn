package sqlitex_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/ionalpha/flynn/internal/sqlitex"
)

// TestDurabilityPragmas asserts the SSD-friendly event-store configuration is
// actually applied: WAL journalling and synchronous=NORMAL. These cut fsyncs and
// write amplification under the agent's append-heavy workload.
func TestDurabilityPragmas(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "x.db")
	db, err := sqlitex.Open(ctx, dsn, testMigrations)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()

	var journal string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journal); err != nil {
		t.Fatalf("journal_mode: %v", err)
	}
	if strings.ToLower(journal) != "wal" {
		t.Fatalf("journal_mode = %q, want wal", journal)
	}

	var sync int
	if err := db.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&sync); err != nil {
		t.Fatalf("synchronous: %v", err)
	}
	if sync != 1 { // 1 == NORMAL
		t.Fatalf("synchronous = %d, want 1 (NORMAL)", sync)
	}
}

// testMigrations is a minimal embedded-style migration set under "migrations",
// matching the layout Open expects (it does fs.Sub(fsys, "migrations")).
var testMigrations = fstest.MapFS{
	"migrations/0001_init.sql": &fstest.MapFile{
		Data: []byte(`CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT NOT NULL);`),
	},
}

// TestOpenMigratesReopensIdempotently checks Open creates the schema, that a
// reopen of the same file sees prior data, and that re-running the migrations on
// an already-migrated database is a no-op rather than an error.
func TestOpenMigratesReopensIdempotently(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "x.db")

	db, err := sqlitex.Open(ctx, dsn, testMigrations)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO t (v) VALUES ('a')`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Reopen: schema migration must be idempotent and data must persist.
	db2, err := sqlitex.Open(ctx, dsn, testMigrations)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = db2.Close() }()
	var n int
	if err := db2.QueryRowContext(ctx, `SELECT count(*) FROM t`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("row count = %d, want 1 (data did not persist across reopen)", n)
	}
}

// TestTxCommitsAndRollsBack checks Tx commits on success and rolls back on error,
// leaving no partial write.
func TestTxCommitsAndRollsBack(t *testing.T) {
	ctx := context.Background()
	db, err := sqlitex.Open(ctx, ":memory:", testMigrations)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()

	// A returning-error transaction must leave the table unchanged.
	sentinel := errors.New("boom")
	err = sqlitex.Tx(ctx, db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO t (v) VALUES ('rolled-back')`); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Tx error = %v, want sentinel", err)
	}
	if got := count(ctx, t, db); got != 0 {
		t.Fatalf("after rollback, rows = %d, want 0", got)
	}

	// A successful transaction commits.
	if err := sqlitex.Tx(ctx, db, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO t (v) VALUES ('committed')`)
		return err
	}); err != nil {
		t.Fatalf("Tx commit: %v", err)
	}
	if got := count(ctx, t, db); got != 1 {
		t.Fatalf("after commit, rows = %d, want 1", got)
	}
}

// TestFormatParseRoundTrip checks the canonical time format round-trips in UTC
// and that a malformed string yields the zero time.
func TestFormatParseRoundTrip(t *testing.T) {
	in := time.Date(2026, 6, 23, 15, 4, 5, 123456789, time.UTC)
	if got := sqlitex.ParseTime(sqlitex.FormatTime(in)); !got.Equal(in) {
		t.Fatalf("round-trip = %v, want %v", got, in)
	}
	// A non-UTC input is normalised to UTC by FormatTime.
	loc := time.FixedZone("x", 7*3600)
	off := time.Date(2026, 6, 23, 22, 4, 5, 0, loc)
	if got := sqlitex.ParseTime(sqlitex.FormatTime(off)); !got.Equal(off) || got.Location() != time.UTC {
		t.Fatalf("non-UTC round-trip = %v (%v), want equal in UTC", got, got.Location())
	}
	if got := sqlitex.ParseTime("not-a-time"); !got.IsZero() {
		t.Fatalf("ParseTime(bad) = %v, want zero", got)
	}
}

func count(ctx context.Context, t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM t`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

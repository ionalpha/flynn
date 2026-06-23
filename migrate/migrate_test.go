package migrate_test

import (
	"context"
	"database/sql"
	"testing"
	"testing/fstest"

	_ "modernc.org/sqlite" // registers the pure-Go "sqlite" driver

	"github.com/ionalpha/flynn/migrate"
)

// openMem returns an in-memory SQLite handle pinned to a single connection, so
// every statement hits the same :memory: database.
func openMem(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func countApplied(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRunAppliesInOrderAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	db := openMem(t)
	fsys := fstest.MapFS{
		"0002_more.sql": {Data: []byte(`ALTER TABLE a ADD COLUMN name TEXT;`)},
		"0001_init.sql": {Data: []byte(`CREATE TABLE a(id INTEGER PRIMARY KEY); CREATE TABLE b(id INTEGER);`)},
	}

	if err := migrate.Run(ctx, db, fsys); err != nil {
		t.Fatalf("run: %v", err)
	}
	if n := countApplied(t, db); n != 2 {
		t.Fatalf("applied = %d, want 2", n)
	}
	// Both migrations took effect (multi-statement 0001, then the 0002 column).
	if _, err := db.Exec(`INSERT INTO a(id, name) VALUES (1, 'x')`); err != nil {
		t.Fatalf("schema not fully applied: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO b(id) VALUES (1)`); err != nil {
		t.Fatalf("second statement of 0001 not applied: %v", err)
	}

	// Idempotent: a second run is a no-op.
	if err := migrate.Run(ctx, db, fsys); err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if n := countApplied(t, db); n != 2 {
		t.Fatalf("re-run changed applied count to %d", n)
	}
}

func TestRunFailsClosedAndRollsBack(t *testing.T) {
	ctx := context.Background()
	db := openMem(t)
	fsys := fstest.MapFS{
		"0001_ok.sql":  {Data: []byte(`CREATE TABLE a(id INTEGER);`)},
		"0002_bad.sql": {Data: []byte(`CREATE TABLE b(id INTEGER); THIS IS NOT SQL;`)},
	}

	if err := migrate.Run(ctx, db, fsys); err == nil {
		t.Fatal("expected an error from the broken migration")
	}
	// 0001 committed; 0002 rolled back entirely.
	if n := countApplied(t, db); n != 1 {
		t.Fatalf("applied = %d, want 1 (0002 must roll back)", n)
	}
	if _, err := db.Exec(`INSERT INTO b(id) VALUES (1)`); err == nil {
		t.Fatal("table b must not exist after rollback")
	}
}

func TestRunDetectsChecksumDrift(t *testing.T) {
	ctx := context.Background()
	db := openMem(t)
	if err := migrate.Run(ctx, db, fstest.MapFS{
		"0001_a.sql": {Data: []byte(`CREATE TABLE a(id INTEGER);`)},
	}); err != nil {
		t.Fatal(err)
	}
	// The applied migration's content is edited — must be refused.
	err := migrate.Run(ctx, db, fstest.MapFS{
		"0001_a.sql": {Data: []byte(`CREATE TABLE a(id INTEGER, extra TEXT);`)},
	})
	if err == nil {
		t.Fatal("expected a checksum-drift error when an applied migration was edited")
	}
}

func TestRunDetectsOutOfOrder(t *testing.T) {
	ctx := context.Background()
	db := openMem(t)
	if err := migrate.Run(ctx, db, fstest.MapFS{
		"0001_a.sql": {Data: []byte(`CREATE TABLE a(id INTEGER);`)},
		"0003_c.sql": {Data: []byte(`CREATE TABLE c(id INTEGER);`)},
	}); err != nil {
		t.Fatal(err)
	}
	// A 0002 now appears below the latest applied version (3).
	err := migrate.Run(ctx, db, fstest.MapFS{
		"0001_a.sql": {Data: []byte(`CREATE TABLE a(id INTEGER);`)},
		"0002_b.sql": {Data: []byte(`CREATE TABLE b(id INTEGER);`)},
		"0003_c.sql": {Data: []byte(`CREATE TABLE c(id INTEGER);`)},
	})
	if err == nil {
		t.Fatal("expected an out-of-order error for a migration inserted below the latest applied")
	}
}

func TestRunRejectsDuplicateAndBadNames(t *testing.T) {
	ctx := context.Background()
	dup := fstest.MapFS{
		"0001_a.sql": {Data: []byte(`CREATE TABLE a(id INTEGER);`)},
		"0001_b.sql": {Data: []byte(`CREATE TABLE c(id INTEGER);`)},
	}
	if err := migrate.Run(ctx, openMem(t), dup); err == nil {
		t.Fatal("expected a duplicate-version error")
	}

	bad := fstest.MapFS{"init.sql": {Data: []byte(`CREATE TABLE a(id INTEGER);`)}}
	if err := migrate.Run(ctx, openMem(t), bad); err == nil {
		t.Fatal("expected a bad-name error")
	}
}

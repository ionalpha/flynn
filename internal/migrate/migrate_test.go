package migrate_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"testing/fstest"

	_ "modernc.org/sqlite" // registers the pure-Go "sqlite" driver

	"github.com/ionalpha/flynn/internal/migrate"
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
	// The applied migration's content is edited - must be refused.
	err := migrate.Run(ctx, db, fstest.MapFS{
		"0001_a.sql": {Data: []byte(`CREATE TABLE a(id INTEGER, extra TEXT);`)},
	})
	if err == nil {
		t.Fatal("expected a checksum-drift error when an applied migration was edited")
	}
	var incompat *migrate.IncompatibleSchemaError
	if !errors.As(err, &incompat) {
		t.Fatalf("expected *IncompatibleSchemaError, got %T: %v", err, err)
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
	var incompat *migrate.IncompatibleSchemaError
	if !errors.As(err, &incompat) {
		t.Fatalf("expected *IncompatibleSchemaError, got %T: %v", err, err)
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

// TestRunConcurrentProcessesOnOneFresh opens one fresh file database from several
// independent handles (one per goroutine, standing in for one per process) and runs
// the same migrations on all of them at once. A migration that drops a column cannot
// be applied twice, so any caller that applies a version another already applied fails.
func TestRunConcurrentProcessesOnOneFresh(t *testing.T) {
	ctx := context.Background()
	fsys := fstest.MapFS{
		"0001_init.sql":      {Data: []byte(`CREATE TABLE t(id INTEGER PRIMARY KEY, uses TEXT, keep TEXT);`)},
		"0002_move.sql":      {Data: []byte(`ALTER TABLE t ADD COLUMN moved TEXT; UPDATE t SET moved = uses;`)},
		"0003_drop_uses.sql": {Data: []byte(`ALTER TABLE t DROP COLUMN uses;`)},
		"0004_more.sql":      {Data: []byte(`ALTER TABLE t ADD COLUMN extra TEXT;`)},
	}
	path := filepath.Join(t.TempDir(), "fresh.db")
	dsn := path + "?_pragma=busy_timeout(30000)"

	const procs = 8
	dbs := make([]*sql.DB, procs)
	for i := range dbs {
		db, err := sql.Open("sqlite", dsn)
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		t.Cleanup(func() { _ = db.Close() })
		dbs[i] = db
	}

	start := make(chan struct{})
	errs := make([]error, procs)
	var wg sync.WaitGroup
	for i, db := range dbs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs[i] = migrate.Run(ctx, db, fsys)
		}()
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("process %d: %v", i, err)
		}
	}
	if n := countApplied(t, dbs[0]); n != 4 {
		t.Fatalf("applied = %d, want 4", n)
	}
	var distinct int
	if err := dbs[0].QueryRow(`SELECT count(DISTINCT version) FROM schema_migrations`).Scan(&distinct); err != nil {
		t.Fatal(err)
	}
	if distinct != 4 {
		t.Fatalf("distinct versions = %d, want 4", distinct)
	}
}

// TestRunRefusesAVersionRecordedMeanwhileWithAnotherChecksum covers a process that
// loses the race to one running different migration files: the version it was about
// to apply turns up recorded with another checksum once it holds the write lock, and
// it must refuse as it would have before applying anything, not skip it silently.
func TestRunRefusesAVersionRecordedMeanwhileWithAnotherChecksum(t *testing.T) {
	ctx := context.Background()
	db := openMem(t)
	// Applying 0001 stands in for the other process: it records 0002 with a checksum
	// that is not this build's, after Run has already decided 0002 is pending.
	fsys := fstest.MapFS{
		"0001_a.sql": {Data: []byte(`CREATE TABLE a(id INTEGER);
			INSERT INTO schema_migrations(version, name, checksum, applied_at) VALUES (2, '0002_b.sql', 'other-build', 'now');`)},
		"0002_b.sql": {Data: []byte(`CREATE TABLE b(id INTEGER);`)},
	}

	err := migrate.Run(ctx, db, fsys)
	var incompat *migrate.IncompatibleSchemaError
	if !errors.As(err, &incompat) {
		t.Fatalf("expected *IncompatibleSchemaError, got %T: %v", err, err)
	}
	if _, err := db.Exec(`INSERT INTO b(id) VALUES (1)`); err == nil {
		t.Fatal("0002 must not be applied over a differing record")
	}
}

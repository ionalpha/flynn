package sqlitex_test

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/ionalpha/flynn/internal/sqlitex"
)

// TestOpenReadPoolReadsConcurrentlyWithTheWriter checks the read side sees what the writer
// committed and, because WAL gives one writer plus N readers, that several reads can be in
// flight at once rather than queueing behind the single write connection.
func TestOpenReadPoolReadsConcurrentlyWithTheWriter(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "x.db")
	db, err := sqlitex.Open(ctx, dsn, testMigrations)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(ctx, `INSERT INTO t (v) VALUES ('a')`); err != nil {
		t.Fatalf("insert: %v", err)
	}

	pool, err := sqlitex.OpenReadPool(dsn, 4)
	if err != nil {
		t.Fatalf("OpenReadPool: %v", err)
	}
	if pool == nil {
		t.Fatal("OpenReadPool returned no pool for a file database")
	}
	defer func() { _ = pool.Close() }()

	if got := pool.Stats().MaxOpenConnections; got != 4 {
		t.Fatalf("pool max conns = %d, want 4", got)
	}

	// Hold two read transactions open at once: a single-connection pool would deadlock
	// here, so this is the property the pool exists for.
	tx1, err := pool.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("read tx 1: %v", err)
	}
	defer func() { _ = tx1.Rollback() }()
	tx2, err := pool.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("read tx 2 (pool did not serve a second concurrent reader): %v", err)
	}
	defer func() { _ = tx2.Rollback() }()

	var n int
	if err := tx1.QueryRowContext(ctx, `SELECT count(*) FROM t`).Scan(&n); err != nil {
		t.Fatalf("read: %v", err)
	}
	if n != 1 {
		t.Fatalf("reader sees %d rows, want the 1 the writer committed", n)
	}
}

// TestOpenReadPoolIsReadOnly checks the pool is pinned with query_only, so a write issued
// through it is refused instead of racing the single writer connection.
func TestOpenReadPoolIsReadOnly(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "x.db")
	db, err := sqlitex.Open(ctx, dsn, testMigrations)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()

	pool, err := sqlitex.OpenReadPool(dsn, 2)
	if err != nil {
		t.Fatalf("OpenReadPool: %v", err)
	}
	defer func() { _ = pool.Close() }()

	if _, err := pool.ExecContext(ctx, `INSERT INTO t (v) VALUES ('nope')`); err == nil {
		t.Fatal("the read pool accepted a write; query_only was not applied")
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM t`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("refused write still landed: %d rows", n)
	}
}

// TestOpenReadPoolConnCap checks the pool size: a caller's figure is honoured and a
// nonsensical one falls back to the default rather than leaving an unbounded pool.
func TestOpenReadPoolConnCap(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "x.db")
	db, err := sqlitex.Open(ctx, dsn, testMigrations)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()

	for _, tc := range []struct{ in, want int }{
		{8, 8},
		{1, 1},
		{0, 4},
		{-3, 4},
	} {
		pool, err := sqlitex.OpenReadPool(dsn, tc.in)
		if err != nil {
			t.Fatalf("OpenReadPool(%d): %v", tc.in, err)
		}
		if got := pool.Stats().MaxOpenConnections; got != tc.want {
			t.Fatalf("OpenReadPool(%d) max conns = %d, want %d", tc.in, got, tc.want)
		}
		if err := pool.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
	}
}

// TestOpenReadPoolMemoryHasNoReadSide checks the in-memory database, which lives inside its
// single write connection and cannot be opened a second time, reports no pool and no error
// so the caller falls back to the write handle.
func TestOpenReadPoolMemoryHasNoReadSide(t *testing.T) {
	pool, err := sqlitex.OpenReadPool(":memory:", 4)
	if err != nil {
		t.Fatalf("OpenReadPool(:memory:) = error %v, want (nil, nil)", err)
	}
	if pool != nil {
		t.Fatal("OpenReadPool(:memory:) returned a pool; a memory database cannot be reopened")
	}
}

// TestOpenRejectsBrokenMigration checks a migration that does not apply fails the open and
// leaves no usable handle, rather than handing back a database on an unknown schema.
func TestOpenRejectsBrokenMigration(t *testing.T) {
	broken := fstest.MapFS{
		"migrations/0001_init.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE (this is not sql;`)},
	}
	db, err := sqlitex.Open(context.Background(), filepath.Join(t.TempDir(), "x.db"), broken)
	if err == nil {
		_ = db.Close()
		t.Fatal("Open accepted a migration that cannot be applied")
	}
	if db != nil {
		t.Fatal("Open returned a handle alongside an error")
	}
	if !strings.Contains(err.Error(), "sqlitex: migrate") {
		t.Fatalf("error = %v, want it to name the migrate stage", err)
	}
}

// TestTxOnClosedDBNeverRunsFn checks a transaction that cannot even be begun reports the
// error and does not run the caller's function, so a write body never executes outside a
// transaction it believes it is inside.
func TestTxOnClosedDBNeverRunsFn(t *testing.T) {
	ctx := context.Background()
	db, err := sqlitex.Open(ctx, ":memory:", testMigrations)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	ran := false
	err = sqlitex.Tx(ctx, db, func(*sql.Tx) error {
		ran = true
		return nil
	})
	if err == nil {
		t.Fatal("Tx on a closed database returned no error")
	}
	if ran {
		t.Fatal("Tx ran the transaction body although no transaction could be begun")
	}
}

// subFailFS is a filesystem whose Sub fails, which is how Open behaves when the migrations
// tree cannot be entered at all.
type subFailFS struct{ err error }

func (f subFailFS) Open(string) (fs.File, error) { return nil, f.err }
func (f subFailFS) Sub(string) (fs.FS, error)    { return nil, f.err }

// TestOpenRejectsUnusableMigrationsFS checks an unreadable migrations tree fails the open
// and names the stage, instead of silently opening an unmigrated database.
func TestOpenRejectsUnusableMigrationsFS(t *testing.T) {
	sentinel := errors.New("no migrations tree")
	db, err := sqlitex.Open(context.Background(), ":memory:", subFailFS{err: sentinel})
	if err == nil {
		_ = db.Close()
		t.Fatal("Open accepted a migrations filesystem it cannot enter")
	}
	if db != nil {
		t.Fatal("Open returned a handle alongside an error")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want it to wrap the filesystem's own error", err)
	}
	if !strings.Contains(err.Error(), "sqlitex: migrations fs") {
		t.Fatalf("error = %v, want it to name the migrations-fs stage", err)
	}
}

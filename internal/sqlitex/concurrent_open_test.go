package sqlitex_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/ionalpha/flynn/internal/sqlitex"
)

// TestOpenConcurrentlyOnAFreshFile opens one new database file from several handles
// at once, as separate processes do when the first command and a second one start
// together. The first connection of each handle switches the file to WAL, and a
// switch that meets another one in flight fails with SQLITE_BUSY without consulting
// the busy timeout, so Open has to retry it. The non-idempotent migration catches a
// handle applying a version another already applied.
func TestOpenConcurrentlyOnAFreshFile(t *testing.T) {
	ctx := context.Background()
	fsys := fstest.MapFS{
		"migrations/0001_init.sql": {Data: []byte(`CREATE TABLE t (id INTEGER PRIMARY KEY, uses TEXT, v TEXT);`)},
		"migrations/0002_drop.sql": {Data: []byte(`ALTER TABLE t DROP COLUMN uses;`)},
	}
	dsn := filepath.Join(t.TempDir(), "fresh.db")

	const procs = 8
	errs := make([]error, procs)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range procs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			db, err := sqlitex.Open(ctx, dsn, fsys)
			if err == nil {
				err = db.Close()
			}
			errs[i] = err
		}()
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("open %d: %v", i, err)
		}
	}
}

// TestTxThatReadsBeforeItWritesWaitsForAnotherWriter runs the shape that failed when two
// flynn processes seeded the bundled skills together: a transaction reads, another handle
// commits a write, and then the first transaction writes. A deferred transaction took only
// a read snapshot at its first SELECT, and SQLite refuses to upgrade a snapshot another
// writer has moved past: SQLITE_BUSY at once, with no busy_timeout wait. [sqlitex.Tx] has
// to take the write lock at BEGIN, so the second writer waits instead and both commit.
func TestTxThatReadsBeforeItWritesWaitsForAnotherWriter(t *testing.T) {
	ctx := context.Background()
	fsys := fstest.MapFS{"migrations/0001_init.sql": {Data: []byte(`CREATE TABLE t (id INTEGER PRIMARY KEY);`)}}
	dsn := filepath.Join(t.TempDir(), "two.db")
	first, err := sqlitex.Open(ctx, dsn, fsys)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	second, err := sqlitex.Open(ctx, dsn, fsys)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })

	read := make(chan struct{})
	otherDone := make(chan error, 1)
	go func() {
		<-read
		otherDone <- sqlitex.Tx(ctx, second, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `INSERT INTO t (id) VALUES (1)`)
			return err
		})
	}()

	var otherErr error
	otherFinished := false
	err = sqlitex.Tx(ctx, first, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM t`).Scan(&n); err != nil {
			return err
		}
		close(read)
		// With the write lock held from BEGIN the other writer cannot finish until this
		// transaction commits, so the wait runs out; without it the other commits first.
		select {
		case otherErr = <-otherDone:
			otherFinished = true
		case <-time.After(300 * time.Millisecond):
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO t (id) VALUES (2)`)
		return err
	})
	if err != nil {
		t.Fatalf("the transaction that read first: %v", err)
	}
	if !otherFinished {
		otherErr = <-otherDone
	}
	if otherErr != nil {
		t.Fatalf("the other writer: %v", otherErr)
	}
}

// TestOpenGivesUpWhenItsContextEnds: a file another process holds exclusively keeps Open
// retrying, and the caller's deadline, not the retry budget, ends the wait.
func TestOpenGivesUpWhenItsContextEnds(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "held.db")
	holder, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Close() })
	holder.SetMaxOpenConns(1)
	for _, stmt := range []string{`CREATE TABLE x (v)`, `BEGIN EXCLUSIVE`, `INSERT INTO x VALUES (1)`} {
		if _, err := holder.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	fsys := fstest.MapFS{"migrations/0001_init.sql": {Data: []byte(`CREATE TABLE t (id INTEGER PRIMARY KEY);`)}}
	db, err := sqlitex.Open(ctx, dsn, fsys)
	if err == nil {
		_ = db.Close()
		t.Fatal("Open succeeded on a file another connection holds exclusively")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Open = %v, want the caller's deadline", err)
	}
}

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

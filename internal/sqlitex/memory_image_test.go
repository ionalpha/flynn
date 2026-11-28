package sqlitex_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/ionalpha/flynn/internal/sqlitex"
)

// imageMigrations is a set no other test opens, so the first open in
// [TestMemoryOpenReusesTheMigratedImage] is the one that migrates it.
var imageMigrations = fstest.MapFS{
	"migrations/0001_parent.sql": &fstest.MapFile{
		Data: []byte(`CREATE TABLE parent (id INTEGER PRIMARY KEY);`),
	},
	"migrations/0002_child.sql": &fstest.MapFile{
		Data: []byte(`CREATE TABLE child (id INTEGER PRIMARY KEY,
			parent INTEGER NOT NULL REFERENCES parent(id), body TEXT NOT NULL);
			ALTER TABLE child ADD COLUMN note TEXT;`),
	},
}

// TestMemoryOpenReusesTheMigratedImage: a second in-memory open of the same migration
// set is a copy of the first one's freshly migrated database, so it carries the first
// open's migration record, and it is otherwise a store of its own with the pragmas
// every connection gets.
func TestMemoryOpenReusesTheMigratedImage(t *testing.T) {
	ctx := context.Background()
	first := openMemory(t, imageMigrations)
	if _, err := first.ExecContext(ctx, `INSERT INTO parent (id) VALUES (1)`); err != nil {
		t.Fatalf("insert into first: %v", err)
	}
	second := openMemory(t, imageMigrations)

	if a, b := appliedAt(t, first), appliedAt(t, second); a != b {
		t.Fatalf("second open applied_at = %q, first = %q: the second open migrated again instead of loading the image", b, a)
	}
	if schema(t, first) != schema(t, second) {
		t.Fatalf("schemas differ:\nfirst:  %s\nsecond: %s", schema(t, first), schema(t, second))
	}
	var n int
	if err := second.QueryRowContext(ctx, `SELECT count(*) FROM parent`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("second store has %d parent rows, want 0: a write to the first leaked into the image", n)
	}

	for pragma, want := range map[string]string{"foreign_keys": "1", "cache_size": "-8000"} {
		var got string
		if err := second.QueryRowContext(ctx, "PRAGMA "+pragma).Scan(&got); err != nil {
			t.Fatalf("PRAGMA %s: %v", pragma, err)
		}
		if got != want {
			t.Errorf("PRAGMA %s on the loaded image = %s, want %s", pragma, got, want)
		}
	}
	if _, err := second.ExecContext(ctx, `INSERT INTO child (parent, body) VALUES (99, 'x')`); err == nil {
		t.Fatal("a child row with no parent was accepted: foreign keys are off on the loaded image")
	}

	// The image is the size of an empty database; writing well past it must grow it.
	body := strings.Repeat("x", 4096)
	if _, err := second.ExecContext(ctx, `INSERT INTO parent (id) VALUES (1)`); err != nil {
		t.Fatalf("insert parent: %v", err)
	}
	for i := range 500 {
		if _, err := second.ExecContext(ctx, `INSERT INTO child (parent, body) VALUES (1, ?)`, body); err != nil {
			t.Fatalf("insert child %d: %v", i, err)
		}
	}
}

// TestMemoryImageIsPerMigrationSet: a set that differs by one migration gets its own
// schema, never the image of another set.
func TestMemoryImageIsPerMigrationSet(t *testing.T) {
	_ = openMemory(t, imageMigrations)
	other := fstest.MapFS{
		"migrations/0001_parent.sql": imageMigrations["migrations/0001_parent.sql"],
		"migrations/0002_other.sql":  &fstest.MapFile{Data: []byte(`CREATE TABLE other (id INTEGER PRIMARY KEY);`)},
	}
	db := openMemory(t, other)
	got := schema(t, db)
	if strings.Contains(got, "child") || !strings.Contains(got, "other") {
		t.Fatalf("schema of the second set = %s, want table other and no table child", got)
	}
}

// TestNewFileStartsAsTheMigratedImage: a database file that does not exist yet is
// created as a copy of the image, then opened like any file: in WAL mode, private to
// the owner, and keeping what is written to it across a reopen. A file that already
// exists is opened as it is.
func TestNewFileStartsAsTheMigratedImage(t *testing.T) {
	ctx := context.Background()
	mem := openMemory(t, imageMigrations)
	dsn := filepath.Join(t.TempDir(), "x.db")
	db, err := sqlitex.Open(ctx, dsn, imageMigrations)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if a, b := appliedAt(t, mem), appliedAt(t, db); a != b {
		t.Fatalf("new file applied_at = %q, image = %q: the file was migrated instead of copied", b, a)
	}
	var journal string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journal); err != nil {
		t.Fatalf("journal_mode: %v", err)
	}
	if !strings.EqualFold(journal, "wal") {
		t.Fatalf("journal_mode = %q, want wal", journal)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO parent (id) VALUES (7)`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if fi, err := os.Stat(dsn); err != nil || (os.PathSeparator == '/' && fi.Mode().Perm() != 0o600) {
		t.Fatalf("database file: %v, %v; want it private to the owner", fi, err)
	}
	entries, err := os.ReadDir(filepath.Dir(dsn))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Fatalf("the staged copy %s was left beside the database", e.Name())
		}
	}

	again, err := sqlitex.Open(ctx, dsn, imageMigrations)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = again.Close() }()
	var n int
	if err := again.QueryRowContext(ctx, `SELECT count(*) FROM parent WHERE id = 7`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("after reopen the row is gone (count %d): an existing file was replaced by the image", n)
	}
}

func openMemory(t *testing.T, fsys fstest.MapFS) *sql.DB {
	t.Helper()
	db, err := sqlitex.Open(context.Background(), ":memory:", fsys)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func appliedAt(t *testing.T, db *sql.DB) string {
	t.Helper()
	var s string
	if err := db.QueryRow(`SELECT group_concat(applied_at, ',') FROM (SELECT applied_at FROM schema_migrations ORDER BY version)`).Scan(&s); err != nil {
		t.Fatalf("applied_at: %v", err)
	}
	return s
}

func schema(t *testing.T, db *sql.DB) string {
	t.Helper()
	var s string
	if err := db.QueryRow(`SELECT group_concat(sql, ';') FROM (SELECT sql FROM sqlite_master WHERE sql IS NOT NULL ORDER BY name)`).Scan(&s); err != nil {
		t.Fatalf("schema: %v", err)
	}
	return s
}

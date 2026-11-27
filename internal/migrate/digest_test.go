package migrate_test

import (
	"testing"
	"testing/fstest"

	"github.com/ionalpha/flynn/internal/migrate"
)

// TestDigestNamesTheMigrationSet: the digest is equal for equal sets and changes
// with any file's bytes or name, because a cache keyed by it must never hand one
// set's schema to another.
func TestDigestNamesTheMigrationSet(t *testing.T) {
	base := fstest.MapFS{
		"0001_a.sql": {Data: []byte(`CREATE TABLE a (id INTEGER);`)},
		"0002_b.sql": {Data: []byte(`CREATE TABLE b (id INTEGER);`)},
	}
	digest := func(fsys fstest.MapFS) string {
		t.Helper()
		d, err := migrate.Digest(fsys)
		if err != nil {
			t.Fatalf("digest: %v", err)
		}
		return d
	}
	want := digest(base)
	if got := digest(fstest.MapFS{"0002_b.sql": base["0002_b.sql"], "0001_a.sql": base["0001_a.sql"]}); got != want {
		t.Fatalf("an equal set has digest %s, want %s", got, want)
	}
	changed := map[string]fstest.MapFS{
		"edited body":    {"0001_a.sql": base["0001_a.sql"], "0002_b.sql": {Data: []byte(`CREATE TABLE b (id TEXT);`)}},
		"renamed file":   {"0001_a.sql": base["0001_a.sql"], "0002_c.sql": base["0002_b.sql"]},
		"added file":     {"0001_a.sql": base["0001_a.sql"], "0002_b.sql": base["0002_b.sql"], "0003_c.sql": {Data: []byte(`SELECT 1;`)}},
		"removed a file": {"0001_a.sql": base["0001_a.sql"]},
	}
	for name, fsys := range changed {
		if digest(fsys) == want {
			t.Errorf("%s: digest unchanged", name)
		}
	}
	if _, err := migrate.Digest(fstest.MapFS{"bad.sql": {Data: []byte(`SELECT 1;`)}}); err == nil {
		t.Fatal("a migration with no version number was digested, want the error Run gives")
	}
}

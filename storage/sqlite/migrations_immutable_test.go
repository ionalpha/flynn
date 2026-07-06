package sqlite

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"io/fs"
	"os"
	"sort"
	"strings"
	"testing"
)

// migrationManifest is the committed record of every migration's checksum. It lives
// beside the migrations so a review sees a change to it in the same diff.
const migrationManifest = "migrations.sums"

// manifestUpdateRequested reports whether -update was passed. The flag is registered once
// by the shared golden-test helpers, so this reads it rather than redefining it (a second
// registration would panic).
func manifestUpdateRequested() bool {
	f := flag.Lookup("update")
	return f != nil && f.Value.String() == "true"
}

// TestMigrationsAreImmutable enforces the append-only migration contract: once a
// migration's checksum is recorded in migrations.sums, its bytes may never change and it
// may never be removed. A released migration that changed would break every database
// already migrated past it, so this fails CI before such a change can ship. A new
// migration is appended by running with -update. Before the first release the manifest
// may be reset wholesale by deleting migrations.sums and regenerating.
func TestMigrationsAreImmutable(t *testing.T) {
	current := migrationChecksums(t)
	recorded := readMigrationManifest(t)

	if manifestUpdateRequested() {
		// -update is append-only: it records new migrations but refuses to rewrite or
		// drop an entry already committed, so history stays immutable even here.
		for name, sum := range recorded {
			cur, ok := current[name]
			if !ok {
				t.Fatalf("recorded migration %q no longer exists; migrations are immutable (to reset before release, delete %s)", name, migrationManifest)
			}
			if cur != sum {
				t.Fatalf("migration %q changed; migrations are immutable, add a new migration instead (to reset before release, delete %s)", name, migrationManifest)
			}
		}
		writeMigrationManifest(t, current)
		return
	}

	var problems []string
	for name, sum := range recorded {
		switch cur, ok := current[name]; {
		case !ok:
			problems = append(problems, "recorded migration "+name+" is missing")
		case cur != sum:
			problems = append(problems, "migration "+name+" was edited after it was recorded (migrations are immutable; add a new migration instead)")
		}
	}
	for name := range current {
		if _, ok := recorded[name]; !ok {
			problems = append(problems, "new migration "+name+" is not recorded; run: go test ./storage/sqlite -run TestMigrationsAreImmutable -update")
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("migration manifest check failed:\n  %s", strings.Join(problems, "\n  "))
	}
}

func migrationChecksums(t *testing.T) map[string]string {
	t.Helper()
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string]string, len(names))
	for _, n := range names {
		body, err := fs.ReadFile(migrations, n)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(body)
		out[strings.TrimPrefix(n, "migrations/")] = hex.EncodeToString(sum[:])
	}
	return out
}

func readMigrationManifest(t *testing.T) map[string]string {
	t.Helper()
	b, err := os.ReadFile(migrationManifest)
	if os.IsNotExist(err) {
		return map[string]string{}
	}
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		if line = strings.TrimSpace(line); line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			t.Fatalf("malformed manifest line: %q", line)
		}
		out[fields[0]] = fields[1]
	}
	return out
}

func writeMigrationManifest(t *testing.T, sums map[string]string) {
	t.Helper()
	names := make([]string, 0, len(sums))
	for n := range sums {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		b.WriteString(n)
		b.WriteByte(' ')
		b.WriteString(sums[n])
		b.WriteByte('\n')
	}
	if err := os.WriteFile(migrationManifest, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

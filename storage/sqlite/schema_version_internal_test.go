package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestHighestVersion(t *testing.T) {
	got, err := highestVersion([]string{"migrations/0002_b.sql", "migrations/0010_c.sql", "migrations/0001_a.sql"})
	if err != nil || got != 10 {
		t.Fatalf("highestVersion = %d, %v; want 10", got, err)
	}
	if _, err := highestVersion([]string{"migrations/oops.sql"}); err == nil {
		t.Fatal("a migration file with no numeric prefix was accepted")
	}
	if got, err := highestEmbeddedMigration(); err != nil || got == 0 {
		t.Fatalf("highestEmbeddedMigration = %d, %v; want the embedded set's highest", got, err)
	}
}

// TestRefuseNewerSchemaEdgeCases covers a database that has recorded no migration yet
// (nothing to compare, so nothing to refuse) and one that cannot be read at all, where
// the failure must be reported rather than taken for "not newer".
func TestRefuseNewerSchemaEdgeCases(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(ctx, `CREATE TABLE schema_migrations(version INTEGER, name TEXT)`); err != nil {
		t.Fatal(err)
	}
	if err := refuseNewerSchema(ctx, db); err != nil {
		t.Fatalf("empty schema_migrations: %v, want nil", err)
	}
	if _, err := db.ExecContext(ctx, `DROP TABLE schema_migrations`); err != nil {
		t.Fatal(err)
	}
	if err := refuseNewerSchema(ctx, db); err == nil {
		t.Fatal("an unreadable schema_migrations was taken for a store that is not newer")
	}
}

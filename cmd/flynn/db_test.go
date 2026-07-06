package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/internal/migrate"
)

// writeDBFamily creates a fake database family (the main file plus its sidecars) under
// dir, so the backup and reset paths can be exercised without a real store.
func writeDBFamily(t *testing.T, dir string) {
	t.Helper()
	for _, name := range []string{"flynn.db", "flynn.db-wal", "flynn.db-shm", "flynn.db.warm"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDBResetMovesFamilyAsideAndRecreates(t *testing.T) {
	dir := t.TempDir()
	writeDBFamily(t, dir)

	if err := runDB([]string{"reset"}, dir); err != nil {
		t.Fatalf("db reset: %v", err)
	}

	// The live database is gone from the data dir root...
	if _, err := os.Stat(filepath.Join(dir, "flynn.db")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("db reset did not move the database aside")
	}
	// ...and preserved (not deleted) under backup-1.
	backup := filepath.Join(dir, "backup-1")
	for _, name := range []string{"flynn.db", "flynn.db-wal", "flynn.db-shm", "flynn.db.warm"} {
		if _, err := os.Stat(filepath.Join(backup, name)); err != nil {
			t.Fatalf("backup missing %s: %v", name, err)
		}
	}

	// A subsequent open recreates a fresh database in the data dir.
	store, err := openDataStore(context.Background(), dir)
	if err != nil {
		t.Fatalf("open after reset: %v", err)
	}
	_ = store.Close()
	if _, err := os.Stat(filepath.Join(dir, "flynn.db")); err != nil {
		t.Fatalf("a fresh database was not created after reset: %v", err)
	}
}

func TestDBResetOnEmptyDirIsNoError(t *testing.T) {
	if err := runDB([]string{"reset"}, t.TempDir()); err != nil {
		t.Fatalf("db reset on an empty dir: %v", err)
	}
}

func TestDBBackupCopiesWithoutRemoving(t *testing.T) {
	dir := t.TempDir()
	writeDBFamily(t, dir)

	if err := runDB([]string{"backup"}, dir); err != nil {
		t.Fatalf("db backup: %v", err)
	}
	// The original stays in place...
	if _, err := os.Stat(filepath.Join(dir, "flynn.db")); err != nil {
		t.Fatalf("db backup removed the original: %v", err)
	}
	// ...and a copy lands under backup-1.
	if _, err := os.Stat(filepath.Join(dir, "backup-1", "flynn.db")); err != nil {
		t.Fatalf("db backup did not copy the database: %v", err)
	}
}

func TestDBBackupsDoNotCollide(t *testing.T) {
	dir := t.TempDir()
	writeDBFamily(t, dir)
	if err := runDB([]string{"backup"}, dir); err != nil {
		t.Fatal(err)
	}
	if err := runDB([]string{"backup"}, dir); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"backup-1", "backup-2"} {
		if _, err := os.Stat(filepath.Join(dir, d, "flynn.db")); err != nil {
			t.Fatalf("expected %s to hold a backup: %v", d, err)
		}
	}
}

func TestExplainStoreOpenErrorGuidesRecovery(t *testing.T) {
	schemaErr := &migrate.IncompatibleSchemaError{Migration: "0003_resources.sql", Reason: "changed after it was applied (checksum mismatch)"}
	wrapped := explainStoreOpenError(schemaErr, `/data/flynn`)
	msg := wrapped.Error()
	for _, want := range []string{"incompatible build", "flynn db reset", "/data/flynn"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("recovery message missing %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "migrate:") || strings.Contains(msg, "sqlitex") {
		t.Fatalf("recovery message still leaks a raw internal error:\n%s", msg)
	}

	// An unrelated error is passed through untouched.
	other := errors.New("disk full")
	if got := explainStoreOpenError(other, "/data/flynn"); !errors.Is(got, other) {
		t.Fatalf("unrelated error was rewrapped: %v", got)
	}
}

func TestDataDirNameIsolatesDevBuilds(t *testing.T) {
	// The test binary is an unstamped dev build, so the data dir is the dev one, kept
	// apart from a release installation's database.
	if got := dataDirName(); got != "flynn-dev" {
		t.Fatalf("dataDirName() = %q, want flynn-dev for a dev build", got)
	}
}

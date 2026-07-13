package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDispatchPsListsTheLiveProcess checks `flynn ps`: listing registers this process's own
// instance first, so the live process always appears in the view, and a stray argument is
// refused with the usage rather than ignored.
func TestDispatchPsListsTheLiveProcess(t *testing.T) {
	dataDir := t.TempDir()
	if err := dispatchPs(nil, dataDir); err != nil {
		t.Fatalf("dispatchPs: %v", err)
	}
	// The listing wrote the local instance into the durable store, so a second view has
	// something to show and the store is readable back.
	if err := dispatchPs(nil, dataDir); err != nil {
		t.Fatalf("second dispatchPs: %v", err)
	}
	if err := dispatchPs([]string{"extra"}, dataDir); err == nil {
		t.Error("ps takes no arguments, so a stray one must be refused")
	}
}

// TestDispatchStatusReportsTheOverviewAndOneRun checks `flynn status`: with no argument it
// prints the instance and run overview, and with a run reference that names nothing it fails
// on the unresolvable reference rather than printing an empty run.
func TestDispatchStatusReportsTheOverviewAndOneRun(t *testing.T) {
	dataDir := t.TempDir()
	if err := dispatchStatus(nil, dataDir); err != nil {
		t.Fatalf("dispatchStatus: %v", err)
	}
	if err := dispatchStatus([]string{"no-such-run"}, dataDir); err == nil {
		t.Error("a run reference that resolves to nothing must be an error")
	}
}

// TestRunDBPathAndBackup covers the database recovery commands: path prints where the
// database lives, backup copies it without disturbing the original, reset moves it aside so
// the next run recreates it, and neither ever deletes data.
func TestRunDBPathAndBackup(t *testing.T) {
	dataDir := t.TempDir()

	// A reset with no database yet is reported plainly rather than failing.
	if err := runDB([]string{"reset"}, dataDir); err != nil {
		t.Fatalf("reset with no database: %v", err)
	}

	// Create a real database by opening the store the commands act on.
	store, err := openDataStore(context.Background(), dataDir)
	if err != nil {
		t.Fatalf("openDataStore: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	dbPath := dataStoreFile(dataDir)
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("the store did not create a database: %v", err)
	}

	if err := runDB([]string{"path"}, dataDir); err != nil {
		t.Fatalf("db path: %v", err)
	}
	if err := runDB([]string{"backup"}, dataDir); err != nil {
		t.Fatalf("db backup: %v", err)
	}
	// A backup copies the database and leaves the original in place.
	if _, err := os.Stat(dbPath); err != nil {
		t.Errorf("backup must not disturb the database: %v", err)
	}
	backups, err := filepath.Glob(filepath.Join(dataDir, "backup*", "*"))
	if err != nil || len(backups) == 0 {
		t.Fatalf("backup wrote no copy (%v): %v", backups, err)
	}

	// A reset moves the database aside, backed up, so the next run recreates it.
	if err := runDB([]string{"reset"}, dataDir); err != nil {
		t.Fatalf("db reset: %v", err)
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Errorf("reset must move the database aside, stat = %v", err)
	}
	second, err := filepath.Glob(filepath.Join(dataDir, "backup*", "*"))
	if err != nil || len(second) <= len(backups) {
		t.Errorf("reset must keep a copy of what it moved aside, got %v", second)
	}
}

// TestRunDBRefusesWhatItCannotAct checks the two refusals: an unknown or missing subcommand
// prints the usage, and an in-memory data directory has no database file to act on.
func TestRunDBRefusesWhatItCannotAct(t *testing.T) {
	for _, args := range [][]string{nil, {"vacuum"}} {
		err := runDB(args, t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "usage: flynn db") {
			t.Errorf("runDB(%v) = %v, want the usage", args, err)
		}
	}
	if err := runDB([]string{"path"}, ":memory:"); err == nil || !strings.Contains(err.Error(), "in-memory") {
		t.Errorf("an in-memory data dir has no database file, got %v", err)
	}
}

package e2e

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDBBackupPreservesDataAndResetRecreates proves the recovery commands are
// non-destructive through the real binary: a run is recorded, `flynn db backup` copies the
// database aside while the original still lists the run, and `flynn db reset` moves it
// aside (preserved under a backup directory) so the next open is fresh. No user data is
// ever deleted, only relocated.
func TestDBBackupPreservesDataAndResetRecreates(t *testing.T) {
	fake := newFakeOpenAI(t, finalText("left a run behind"))
	in := newInstance(t).withModel(fake)

	res := in.run("-no-learn", "goal", "record a run")
	requireExit(t, res, 0, "goal")
	runID := in.runID(res)

	// Backup copies the database; the original is untouched and still lists the run.
	requireExit(t, in.run("db", "backup"), 0, "db backup")
	if _, err := os.Stat(filepath.Join(in.dataDir, "backup-1", "flynn.db")); err != nil {
		t.Fatalf("db backup did not create a copy: %v", err)
	}
	requireContains(t, in.run("runs").stdout, runID, "backup left the original intact")

	// Reset moves the database aside (into the next backup dir) so the next open is fresh;
	// the old database is preserved, not deleted.
	requireExit(t, in.run("db", "reset"), 0, "db reset")
	requireContains(t, in.run("runs").stdout, "no runs yet", "reset produced a fresh database")
	if _, err := os.Stat(filepath.Join(in.dataDir, "backup-2", "flynn.db")); err != nil {
		t.Fatalf("db reset did not preserve the old database: %v", err)
	}
}

// TestDBPathReportsDatabase checks the inspection command names the database file.
func TestDBPathReportsDatabase(t *testing.T) {
	in := newInstance(t)
	res := in.run("db", "path")
	requireExit(t, res, 0, "db path")
	requireContains(t, res.stdout, "flynn.db", "db path shows the database file")
}

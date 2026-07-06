package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
)

// runDB handles `flynn db <path|backup|reset>`: inspect and recover the durable state
// database. Neither backup nor reset ever deletes data. reset moves the current database
// aside so the next run recreates a fresh one, which is the recovery for a database left
// by an incompatible build; backup copies it without disturbing it.
func runDB(args []string, dataDir string) error {
	const usage = "usage: flynn db <path|backup|reset>\n" +
		"  path    print the data directory and database path\n" +
		"  backup  copy the database into a new backup directory\n" +
		"  reset   move the database aside (backed up) so the next run recreates it"
	if len(args) < 1 {
		return errors.New(usage)
	}
	if dataDir == "" || dataDir == ":memory:" {
		return errors.New("db: the data directory is in-memory; there is no database file")
	}
	dbPath := dataStoreFile(dataDir)

	switch args[0] {
	case "path":
		_, _ = fmt.Fprintf(os.Stdout, "data dir: %s\ndatabase: %s\n", dataDir, dbPath)
		return nil
	case "backup":
		dir, err := backupDBFamily(dataDir, dbPath, false)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(os.Stdout, "database copied to %s\n", dir)
		return nil
	case "reset":
		dir, err := backupDBFamily(dataDir, dbPath, true)
		if err != nil {
			return err
		}
		if dir == "" {
			_, _ = fmt.Fprintln(os.Stdout, "no database found; a fresh one is created on the next run")
			return nil
		}
		_, _ = fmt.Fprintf(os.Stdout, "database backed up to %s\na fresh one is created on the next run\n", dir)
		return nil
	default:
		return errors.New(usage)
	}
}

// dbFamily lists the files that make up one SQLite database beside dbPath: the database
// itself, its write-ahead-log sidecars, and the warm read cache with its own sidecars.
// Only files that exist are acted on.
func dbFamily(dbPath string) []string {
	return []string{
		dbPath, dbPath + "-wal", dbPath + "-shm",
		dbPath + ".warm", dbPath + ".warm-wal", dbPath + ".warm-shm",
	}
}

// backupDBFamily copies (move=false) or moves (move=true) every existing member of the
// database family into a fresh backup directory under dataDir, preserving file names so
// the set can be restored together. It returns the backup directory, or "" when there was
// no database to act on. A move leaves the data dir without a database, so the next open
// creates a fresh one.
func backupDBFamily(dataDir, dbPath string, move bool) (string, error) {
	present := make([]string, 0, len(dbFamily(dbPath)))
	for _, f := range dbFamily(dbPath) {
		if _, err := os.Stat(f); err == nil {
			present = append(present, f)
		}
	}
	if len(present) == 0 {
		return "", nil
	}
	dir, err := nextBackupDir(dataDir)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	for _, src := range present {
		dst := filepath.Join(dir, filepath.Base(src))
		if move {
			if err := os.Rename(src, dst); err != nil {
				return "", fmt.Errorf("db: move %s: %w", filepath.Base(src), err)
			}
		} else if err := copyFile(src, dst); err != nil {
			return "", fmt.Errorf("db: copy %s: %w", filepath.Base(src), err)
		}
	}
	return dir, nil
}

// nextBackupDir returns the first unused "backup-N" directory under dataDir, so repeated
// backups never overwrite an earlier one. It avoids a wall-clock name so the choice does
// not depend on the time source.
func nextBackupDir(dataDir string) (string, error) {
	for n := 1; n < 100000; n++ {
		dir := filepath.Join(dataDir, "backup-"+strconv.Itoa(n))
		if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
			return dir, nil
		}
	}
	return "", errors.New("db: too many existing backups")
}

// copyFile copies src to dst with owner-only permissions, so a database backup does not
// land with looser access than the original.
func copyFile(src, dst string) error {
	in, err := os.Open(src) //nolint:gosec // src is a database file under the operator's own data dir
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600) //nolint:gosec // dst is a backup path under the operator's own data dir
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

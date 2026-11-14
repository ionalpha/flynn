package sqlite_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/ionalpha/flynn/storage/sqlite"
)

// A property test opens a store per generated case so no case sees another's rows.
// Opening replays every migration, and each ALTER and DROP makes SQLite re-parse the
// whole schema; under the race detector that is hundreds of milliseconds, and a
// property that runs a hundred cases spent nearly all of its time there. Migrating
// once and copying the resulting file gives each case an independent store whose Open
// finds the schema current and has no migration to run.
//
// The template is produced by [sqlite.Open] itself, so it carries exactly the schema
// the migrations produce and cannot drift from them.

var (
	templateOnce sync.Once
	templateDir  string
	templateFile string
	errTemplate  error
)

// TestMain removes the template directory once every test in the package has run.
func TestMain(m *testing.M) {
	code := m.Run()
	if templateDir != "" {
		_ = os.RemoveAll(templateDir)
	}
	os.Exit(code)
}

// migratedTemplate returns the path of a database file with every migration applied.
// Closing the last connection checkpoints the WAL into the main file, so copying that
// one file copies the whole database.
func migratedTemplate() (string, error) {
	templateOnce.Do(func() {
		dir, err := os.MkdirTemp("", "flynn-sqlite-template-")
		if err != nil {
			errTemplate = err
			return
		}
		templateDir = dir
		path := filepath.Join(dir, "template.db")
		s, err := sqlite.Open(context.Background(), path)
		if err != nil {
			errTemplate = fmt.Errorf("open template: %w", err)
			return
		}
		if err := s.Close(); err != nil {
			errTemplate = fmt.Errorf("close template: %w", err)
			return
		}
		templateFile = path
	})
	return templateFile, errTemplate
}

// openFresh opens an empty, fully migrated store in a directory of its own and returns
// it with the function that closes it and removes the directory. It reports failure
// through fatalf so a rapid case and a plain test can both use it.
func openFresh(fatalf func(format string, args ...any)) (*sqlite.Store, func()) {
	tmpl, err := migratedTemplate()
	if err != nil {
		fatalf("migrated template: %v", err)
	}
	body, err := os.ReadFile(tmpl)
	if err != nil {
		fatalf("read template: %v", err)
	}
	dir, err := os.MkdirTemp("", "flynn-sqlite-case-")
	if err != nil {
		fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "flynn.db"), body, 0o600); err != nil {
		_ = os.RemoveAll(dir)
		fatalf("copy template: %v", err)
	}
	s, err := sqlite.Open(context.Background(), filepath.Join(dir, "flynn.db"))
	if err != nil {
		_ = os.RemoveAll(dir)
		fatalf("open: %v", err)
	}
	return s, func() {
		_ = s.Close()
		_ = os.RemoveAll(dir)
	}
}

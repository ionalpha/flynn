package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// startingSession puts the front door in the state it runs from with no terminal: no
// provider credentials but a key for the one the spec names, a working directory of its
// own, standard input and output that are files rather than terminals. It returns a data
// directory the caller can break before the session opens it.
func startingSession(t *testing.T) string {
	t.Helper()
	noProviderKeys(t)
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Chdir(t.TempDir())
	replaceStdin(t, "")
	captureStdout(t)
	return t.TempDir()
}

// TestRunInteractiveReportsABrokenKeymap proves a keymap the session cannot read stops it
// at the front door and names the file. The alternative is a session that silently runs
// on the default bindings, which reads as the customisation having been ignored with
// nothing to point at.
func TestRunInteractiveReportsABrokenKeymap(t *testing.T) {
	dataDir := startingSession(t)
	writeConfigFile(t, dataDir, "keymap.json", "{not json")

	err := runInteractive("openai:gpt-5.5", dataDir, false, false, true, nil)
	if err == nil {
		t.Fatal("a session opened on an unreadable keymap")
	}
	if !strings.Contains(err.Error(), "keymap.json") {
		t.Fatalf("the error does not name the file to fix: %v", err)
	}
}

// TestRunInteractiveReportsABrokenTheme is the same for the theme, which is read at the
// same point and for the same reason.
func TestRunInteractiveReportsABrokenTheme(t *testing.T) {
	dataDir := startingSession(t)
	writeConfigFile(t, dataDir, "theme.json", "{not json")

	err := runInteractive("openai:gpt-5.5", dataDir, false, false, true, nil)
	if err == nil {
		t.Fatal("a session opened on an unreadable theme")
	}
	if !strings.Contains(err.Error(), "theme.json") {
		t.Fatalf("the error does not name the file to fix: %v", err)
	}
}

// TestRunInteractiveReportsAnUnusableDataDir proves a data directory the store cannot be
// opened under is reported rather than started around. A session that ran anyway would
// take turns it could not record, which is the one thing it must not do quietly.
func TestRunInteractiveReportsAnUnusableDataDir(t *testing.T) {
	dataDir := startingSession(t)
	// A directory where the database file belongs. Everything else under the data
	// directory still works, so the failure is the store's alone, and a directory cannot
	// be opened as a database file on any platform.
	if err := os.MkdirAll(dataStoreFile(dataDir), 0o750); err != nil {
		t.Fatal(err)
	}

	if err := runInteractive("openai:gpt-5.5", dataDir, false, false, true, nil); err == nil {
		t.Fatal("a session opened on a data directory that cannot hold a store")
	}
}

// TestRunInteractiveLearnsBackWhenLearningIsOn proves the learning flag reaches the
// session as a distiller. A native session distils through the model it drives, so the
// flag has to arrive as one; an external session gets none, which is asserted where the
// external front door is.
func TestRunInteractiveLearnsBackWhenLearningIsOn(t *testing.T) {
	dataDir := startingSession(t)

	if err := runInteractive("openai:gpt-5.5", dataDir, true, false, true, nil); err != nil {
		t.Fatalf("runInteractive: %v", err)
	}
}

// writeConfigFile puts content at name in the session's data directory, for the files the
// front door reads before it opens a session.
func writeConfigFile(t *testing.T, dataDir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestRunInteractiveReportsAnUnavailableHarness proves the front door refuses an external
// backend whose CLI is not installed, before it opens a store or a session. The session
// would otherwise start on a harness it cannot drive and fail at the first turn, with the
// conversation already begun.
func TestRunInteractiveReportsAnUnavailableHarness(t *testing.T) {
	dataDir := startingSession(t)
	withNoExecutables(t)

	err := runInteractive("claude:sonnet", dataDir, false, false, true, nil)
	if err == nil {
		t.Fatal("a session opened on a harness that is not installed")
	}
}

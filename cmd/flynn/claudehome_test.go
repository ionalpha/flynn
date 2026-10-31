package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestMain points the claude and codex backends' homes at temporary directories for the
// whole package. Building either spawner creates its home, and without this every test that
// assembles one would create it under the real user's data directory.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "flynn-agent-homes-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	claudeHome = func() string { return filepath.Join(dir, "claude") }
	codexHome = func() string { return filepath.Join(dir, "codex") }
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

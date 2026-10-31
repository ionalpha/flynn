package main

import (
	"fmt"
	"os"
	"testing"
)

// TestMain points the claude backend's Claude Code home at a temporary directory for the
// whole package. Building a claude spawner creates that home, and without this every test
// that assembles one would create it under the real user's config directory.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "flynn-claude-home-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	claudeHome = func() string { return dir }
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

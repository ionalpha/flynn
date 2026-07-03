package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadThemeMissingFileSelectsDefault: no theme.json means the default
// theme, signalled by a nil return the session turns into theme.Default.
func TestLoadThemeMissingFileSelectsDefault(t *testing.T) {
	th, err := loadTheme(t.TempDir())
	if err != nil {
		t.Fatalf("no theme.json should not error: %v", err)
	}
	if th != nil {
		t.Fatalf("no theme.json should select the default (nil), got %q", th.Name())
	}
}

// TestLoadThemeReadsAndLayersAUserFile: a theme.json is parsed and layered
// over its declared base, so the session runs the user's named theme.
func TestLoadThemeReadsAndLayersAUserFile(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, `{"name":"midnight","base":"mono","styles":{"error":{"fg":"red","bold":true}}}`)
	th, err := loadTheme(dir)
	if err != nil {
		t.Fatalf("loadTheme: %v", err)
	}
	if th == nil || th.Name() != "midnight" {
		t.Fatalf("theme = %v, want the user's midnight theme", th)
	}
}

// TestLoadThemeReportsAParseErrorWithTheFile: a broken theme.json fails the
// session start and names the file, since a silent default would leave a
// user's theme quietly ignored.
func TestLoadThemeReportsAParseErrorWithTheFile(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, `{"styles":{"not.a.role":{}}}`)
	_, err := loadTheme(dir)
	if err == nil {
		t.Fatal("an unknown role should fail loadTheme")
	}
	if !strings.Contains(err.Error(), "theme.json") {
		t.Fatalf("error should name the file, got %q", err)
	}
}

func write(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "theme.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

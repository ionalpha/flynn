package fsatomic

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestWriteFileCreatesWithPerm(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.json")
	if err := WriteFile(path, []byte(`{"a":1}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != `{"a":1}` {
		t.Fatalf("content = %q", got)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("Stat: %v", err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("perm = %o, want 600", perm)
		}
	}
}

func TestWriteFileReplacesExisting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.json")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(path, []byte("new"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "new" {
		t.Fatalf("content = %q, want new", got)
	}
}

func TestWriteFileLeavesNoTempOnSuccess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.json")
	if err := WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
	if len(entries) != 1 {
		t.Fatalf("dir entries = %d, want 1", len(entries))
	}
}

func TestWriteFileMissingDirFailsCleanly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nope", "out.json")
	if err := WriteFile(path, []byte("x"), 0o600); err == nil {
		t.Fatal("expected error for missing directory")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file should not exist, stat err = %v", err)
	}
}

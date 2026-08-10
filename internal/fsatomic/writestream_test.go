package fsatomic

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestWriteStreamCommitsWhatWasStreamed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "profile.txt")

	err := WriteStream(path, 0o600, func(w io.Writer) error {
		for _, chunk := range []string{"first", " ", "second"} {
			if _, err := io.WriteString(w, chunk); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WriteStream: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "first second" {
		t.Fatalf("content = %q, want %q", got, "first second")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("dir entries = %d, want only the committed file", len(entries))
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

// A producer that fails halfway has already written bytes into the temp file. Those
// bytes must not reach the destination, and the previous contents must survive: the
// caller streams precisely because it cannot check the content before writing it.
func TestWriteStreamAbandonsAPartialProducer(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "profile.txt")
	if err := os.WriteFile(path, []byte("previous"), 0o600); err != nil {
		t.Fatal(err)
	}

	sentinel := errors.New("producer gave up")
	err := WriteStream(path, 0o600, func(w io.Writer) error {
		if _, werr := io.WriteString(w, "half a prof"); werr != nil {
			return werr
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("WriteStream error = %v, want %v", err, sentinel)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "previous" {
		t.Fatalf("content = %q, want the previous contents untouched", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Fatalf("abandoned write left %s behind", e.Name())
		}
	}
}

func TestWriteStreamMissingDirFailsCleanly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nope", "profile.txt")
	called := false
	err := WriteStream(path, 0o600, func(io.Writer) error {
		called = true
		return nil
	})
	if err == nil {
		t.Fatal("expected an error for a missing directory")
	}
	if called {
		t.Error("the producer ran even though no temp file could be created")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file should not exist, stat err = %v", err)
	}
}

package fsatomic

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// errDisk stands in for the failure of a filesystem operation that cannot be provoked
// through a real disk in a test: a full volume, a lost handle, an IO error on flush.
var errDisk = errors.New("no space left on device")

// step names the operation on the temp file that faultyFile fails at.
type step int

const (
	stepWrite step = iota
	stepChmod
	stepSync
	stepClose
)

func (s step) String() string {
	return [...]string{"write", "chmod", "sync", "close"}[s]
}

// faultyFile is a real temp file whose named step fails. Everything else behaves normally,
// so the write path runs exactly as it does in production up to the injected failure and the
// cleanup that follows is the real one.
type faultyFile struct {
	*os.File
	fail   step
	closed bool
}

func (f *faultyFile) Write(p []byte) (int, error) {
	if f.fail == stepWrite {
		return 0, errDisk
	}
	return f.File.Write(p)
}

func (f *faultyFile) Chmod(mode os.FileMode) error {
	if f.fail == stepChmod {
		return errDisk
	}
	return f.File.Chmod(mode)
}

func (f *faultyFile) Sync() error {
	if f.fail == stepSync {
		return errDisk
	}
	return f.File.Sync()
}

// Close fails on the first call when that is the injected step, so the deferred cleanup can
// still close the real handle and let the temp file be removed.
func (f *faultyFile) Close() error {
	if f.fail == stepClose && !f.closed {
		f.closed = true
		_ = f.File.Close()
		return errDisk
	}
	return f.File.Close()
}

// injectFailure points createTemp at a file that fails the given step, restoring the real
// one afterwards. createTemp is package-level state, so a test using it must not run in
// parallel with another that does.
func injectFailure(t *testing.T, at step) {
	t.Helper()
	old := createTemp
	t.Cleanup(func() { createTemp = old })
	createTemp = func(dir, pattern string) (tempFile, error) {
		f, err := os.CreateTemp(dir, pattern)
		if err != nil {
			return nil, err
		}
		return &faultyFile{File: f, fail: at}, nil
	}
}

// TestWriteFilePreservesPreviousContentsOnFailure checks the durability contract at every
// step that can fail: the destination still holds the previous bytes, and no temp file is
// left in the directory for the next write to trip over.
func TestWriteFilePreservesPreviousContentsOnFailure(t *testing.T) {
	for _, at := range []step{stepWrite, stepChmod, stepSync, stepClose} {
		t.Run(at.String(), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "out.json")
			if err := WriteFile(path, []byte("old"), 0o600); err != nil {
				t.Fatalf("seed write: %v", err)
			}

			injectFailure(t, at)
			err := WriteFile(path, []byte("new"), 0o600)
			if !errors.Is(err, errDisk) {
				t.Fatalf("WriteFile failing at %v returned %v, want the disk error", at, err)
			}

			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("ReadFile: %v", err)
			}
			if string(got) != "old" {
				t.Fatalf("after a failure at %v the file holds %q, want the previous contents", at, got)
			}
			assertNoTempLeft(t, dir)
		})
	}
}

// TestWriteFileCreatesNothingOnFailure checks a first-ever write that fails leaves the
// destination absent rather than empty or partial: a reader must not find a truncated file
// where none existed.
func TestWriteFileCreatesNothingOnFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.json")

	injectFailure(t, stepWrite)
	if err := WriteFile(path, []byte("payload"), 0o600); !errors.Is(err, errDisk) {
		t.Fatalf("WriteFile = %v, want the disk error", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("destination exists after a failed first write, stat err = %v", err)
	}
	assertNoTempLeft(t, dir)
}

// TestWriteFileRenameFailureLeavesNoTemp checks the failure of the rename itself, the last
// step that can fail: writing over a path already held by a directory cannot succeed, and
// the temp file must not survive the attempt.
func TestWriteFileRenameFailureLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "occupied")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "child"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := WriteFile(target, []byte("data"), 0o600); err == nil {
		t.Fatal("WriteFile replaced a non-empty directory")
	}
	// The directory and its contents are untouched.
	if got, err := os.ReadFile(filepath.Join(target, "child")); err != nil || string(got) != "x" {
		t.Fatalf("directory contents changed: %q, err %v", got, err)
	}
	assertNoTempLeft(t, dir)
}

// assertNoTempLeft checks the failed write cleaned up after itself: a leftover temp file
// would accumulate on every failure and leave half-written data on disk.
func assertNoTempLeft(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Fatalf("temp file left behind after a failed write: %s", e.Name())
		}
	}
}

// TestCreateTempFailureIsReported checks the very first step: when the temp file cannot be
// opened at all, the error is returned and nothing is written.
func TestCreateTempFailureIsReported(t *testing.T) {
	old := createTemp
	t.Cleanup(func() { createTemp = old })
	createTemp = func(string, string) (tempFile, error) { return nil, errDisk }

	dir := t.TempDir()
	path := filepath.Join(dir, "out.json")
	if err := WriteFile(path, []byte("x"), 0o600); !errors.Is(err, errDisk) {
		t.Fatalf("WriteFile = %v, want the disk error", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file exists although the temp file was never created, stat err = %v", err)
	}
}

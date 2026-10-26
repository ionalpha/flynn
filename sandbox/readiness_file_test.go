package sandbox

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestReadinessFileNotWritableFromSandbox holds the readiness file out of every path a
// confined command can write: the working directory, a granted writable directory, and
// the scratch areas. A path reached through a symbolic link is judged by where it lands.
func TestReadinessFileNotWritableFromSandbox(t *testing.T) {
	work, granted, state := t.TempDir(), t.TempDir(), t.TempDir()
	l, err := NewLocal(work, WithWritableDir(granted))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })

	refused := map[string]string{
		"working directory":      filepath.Join(l.Root(), "ready.json"),
		"granted writable dir":   filepath.Join(granted, "ready.json"),
		"nested under the grant": filepath.Join(granted, "sub", "ready.json"),
	}
	if err := os.Mkdir(filepath.Join(granted, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(state, "link")
	if runtime.GOOS != "windows" {
		if err := os.Symlink(work, link); err != nil {
			t.Fatal(err)
		}
		refused["symlink into the working directory"] = filepath.Join(link, "ready.json")
	}
	for name, path := range refused {
		err := WriteReadinessFile(path, Readiness{Ready: true}, l.WritableRoots()...)
		if !errors.Is(err, ErrReadinessFileWritable) {
			t.Errorf("%s: err = %v, want ErrReadinessFileWritable", name, err)
		}
		if _, statErr := os.Stat(path); statErr == nil {
			t.Errorf("%s: a file was written although the write was refused", name)
		}
	}

	// A test directory lives under the scratch areas on Linux, so the acceptance case sets
	// them aside; the next test holds them to being refused.
	prev := scratchRoots
	scratchRoots = nil
	t.Cleanup(func() { scratchRoots = prev })
	ok := filepath.Join(state, "ready.json")
	if err := WriteReadinessFile(ok, Readiness{Cause: CauseUnknown, Tier: tierFloor}, l.WritableRoots()...); err != nil {
		t.Fatalf("a path outside every writable root must be accepted: %v", err)
	}
	b, err := os.ReadFile(ok)
	if err != nil {
		t.Fatal(err)
	}
	var got Readiness
	if err := json.Unmarshal(b, &got); err != nil || got.Ready || got.Cause != CauseUnknown {
		t.Fatalf("the record did not round-trip: %v %+v", err, got)
	}
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(ok); err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("the readiness file must be private to its owner: %v %v", fi, err)
		}
	}
}

// TestReadinessFileRefusesTheScratchAreas covers the directories the confined view leaves
// writable besides the working directory.
func TestReadinessFileRefusesTheScratchAreas(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the scratch areas are POSIX paths")
	}
	for _, dir := range scratchRoots {
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			continue
		}
		err := WriteReadinessFile(filepath.Join(dir, "flynn-ready-test.json"), Readiness{Ready: true})
		if !errors.Is(err, ErrReadinessFileWritable) {
			t.Errorf("%s: err = %v, want ErrReadinessFileWritable", dir, err)
		}
	}
}

// TestReadinessFileNeedsAnExistingDirectory shows a path whose directory is missing fails
// before anything is written, rather than creating a directory the caller did not vet
// against the writable roots.
func TestReadinessFileNeedsAnExistingDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "ready.json")
	if err := WriteReadinessFile(path, Readiness{Ready: true}); err == nil {
		t.Fatal("a missing directory must fail the write")
	}
	if _, err := os.Stat(filepath.Dir(path)); err == nil {
		t.Fatal("the directory was created")
	}
}

// TestPathWithinIsFalseForPathsThatCannotBeRelated covers a relative path against an
// absolute one: with no relation between them, one is not inside the other.
func TestPathWithinIsFalseForPathsThatCannotBeRelated(t *testing.T) {
	abs, err := filepath.Abs("parent")
	if err != nil {
		t.Fatal(err)
	}
	if pathWithin("relative", abs) {
		t.Fatal("a relative path was reported inside an absolute one")
	}
}

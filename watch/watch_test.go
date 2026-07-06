package watch

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
)

// newTestWatcher builds a Watcher over root without starting its polling goroutine,
// so a test drives tick directly and asserts on one deterministic scan at a time.
func newTestWatcher(root string, handle Handler) *Watcher {
	return &Watcher{
		root:    root,
		ignore:  loadIgnore(root),
		maxSize: defaultMaxFileSize,
		seen:    map[string]struct{}{},
		handle:  handle,
	}
}

func writeFile(t *testing.T, root, rel, content string) string {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestTickFiresClearsAndDedups(t *testing.T) {
	root := t.TempDir()
	p := writeFile(t, root, "main.go", "package main\nvar x = 1 // ai! rename to count\n")

	var fired []Marker
	w := newTestWatcher(root, func(m Marker) error { fired = append(fired, m); return nil })

	w.tick()
	if len(fired) != 1 {
		t.Fatalf("first tick fired %d markers, want 1", len(fired))
	}
	m := fired[0]
	if m.File != "main.go" || m.Line != 2 || m.Kind != Act || m.Text != "rename to count" {
		t.Errorf("marker = %+v", m)
	}
	if got, want := m.Provenance(), "main.go:2 (ai!)"; got != want {
		t.Errorf("provenance = %q, want %q", got, want)
	}

	// The marker is cleared from the file, keeping the code before it.
	got, _ := os.ReadFile(p)
	if want := "package main\nvar x = 1\n"; string(got) != want {
		t.Errorf("file after clear =\n%q\nwant\n%q", got, want)
	}

	// A second tick sees no marker (cleared) and does not refire.
	w.tick()
	if len(fired) != 1 {
		t.Fatalf("second tick refired; total %d, want 1", len(fired))
	}
}

func TestTickRefiresAfterReadd(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.go", "x := 1 // ai! do it\n")

	var count int
	w := newTestWatcher(root, func(_ Marker) error { count++; return nil })

	w.tick() // fires and clears
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}
	// Re-add the identical marker: it must fire again because the prior one left the
	// tree when it was cleared.
	writeFile(t, root, "a.go", "x := 1 // ai! do it\n")
	w.tick()
	if count != 2 {
		t.Fatalf("count after re-add = %d, want 2", count)
	}
}

func TestTickHandlerErrorLeavesMarker(t *testing.T) {
	root := t.TempDir()
	p := writeFile(t, root, "a.go", "x := 1 // ai! do it\n")

	var count int
	w := newTestWatcher(root, func(_ Marker) error { count++; return os.ErrPermission })

	w.tick()
	w.tick()
	if count != 1 {
		t.Fatalf("count = %d, want 1 (a failed marker is not retried until edited)", count)
	}
	// The file is untouched, so the marker is still there to retry once edited.
	got, _ := os.ReadFile(p)
	if want := "x := 1 // ai! do it\n"; string(got) != want {
		t.Errorf("file = %q, want unchanged %q", got, want)
	}
}

func TestScanSkipsIgnoredAndBinary(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, ".gitignore", "ignored/\n*.bin\n")
	writeFile(t, root, "ignored/x.go", "y := 1 // ai! nope\n")
	writeFile(t, root, "keep.go", "z := 1 // ai! yes\n")
	// A binary-looking file with a NUL byte is skipped even without an ignore rule.
	writeFile(t, root, "blob.dat", "\x00\x01 // ai! nope\n")

	w := newTestWatcher(root, nil)
	got := w.scan()
	if len(got) != 1 {
		t.Fatalf("scan found %d markers, want 1: %+v", len(got), got)
	}
	for _, m := range got {
		if m.File != "keep.go" {
			t.Errorf("scanned unexpected file %q", m.File)
		}
	}
}

func TestStartStopLifecycle(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.go", "x := 1 // ai! go\n")

	fired := make(chan Marker, 1)
	var once sync.Once
	w := Start(clock.System{}, root, 5*time.Millisecond, func(m Marker) error {
		once.Do(func() { fired <- m })
		return nil
	})
	defer w.Stop()

	select {
	case m := <-fired:
		if m.File != "a.go" || m.Kind != Act {
			t.Errorf("fired marker = %+v", m)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("watcher did not fire the marker within 2s")
	}
}

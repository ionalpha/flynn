package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFloorRecordWritesToStderrWithoutANotice shows a run at the floor is never silent: with
// no notice configured the reason goes to standard error, and the run is still counted.
func TestFloorRecordWritesToStderrWithoutANotice(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	prev := os.Stderr
	os.Stderr = f
	t.Cleanup(func() { os.Stderr = prev })

	p := &floorPolicy{optedIn: true}
	p.record(errors.New("no user namespaces"))
	os.Stderr = prev

	b, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "no user namespaces") || !strings.Contains(string(b), "floor") {
		t.Fatalf("stderr = %q, want the floor notice with the reason", b)
	}
	if p.runs.Load() != 1 {
		t.Fatalf("runs = %d, want 1", p.runs.Load())
	}
}

package testkit

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/ionalpha/flynn/dispatch"
)

var updateGolden = flag.Bool("update", false, "update golden files in testdata/")

// Diff fails the test with a readable diff when want != got. It is the generic
// comparison primitive for any type, so packages don't write a bespoke DiffXxx;
// nil and empty slices/maps compare equal.
func Diff[T any](t TB, want, got T) {
	t.Helper()
	if d := cmp.Diff(want, got, cmpopts.EquateEmpty()); d != "" {
		t.Fatalf("mismatch (-want +got):\n%s", d)
	}
}

// DiffEvents is Diff specialised to a dispatch-event stream, kept for
// readability at call sites. Two runs of the same scenario under a clock.Manual
// produce byte-identical streams, so this is also the determinism/replay check.
func DiffEvents(t TB, want, got []dispatch.Event) {
	t.Helper()
	Diff(t, want, got)
}

// Golden compares got against testdata/<name>.golden (pretty JSON). Run the
// tests with -update to (re)write the file. It lets an entire output — a full
// mission replay, a rendered spec, a whole event stream — be a single snapshot
// with no hand-written expected value, which is what keeps large tests small.
func Golden[T any](t TB, name string, got T) {
	t.Helper()
	data, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatalf("golden %q: marshal: %v", name, err)
	}
	data = append(data, '\n')
	// Confine the file to testdata/; name is a test-controlled label, not a path.
	path := filepath.Join("testdata", filepath.Base(name)+".golden")

	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o750); err != nil {
			t.Fatalf("golden %q: mkdir: %v", name, err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatalf("golden %q: write: %v", name, err)
		}
		return
	}
	want, err := os.ReadFile(path) //nolint:gosec // G304: path confined to testdata/ above; test-only helper
	if err != nil {
		t.Fatalf("golden %q: %v (run the tests with -update to create it)", name, err)
	}
	if !bytes.Equal(want, data) {
		t.Fatalf("golden %q mismatch (run -update to accept):\n%s", name, data)
	}
}

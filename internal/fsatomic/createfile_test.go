package fsatomic_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/ionalpha/flynn/internal/fsatomic"
)

// TestCreateFileNeverReplaces: CreateFile writes a new file whole, refuses a name that
// is taken without touching what is there, and leaves no staged copy either way.
func TestCreateFileNeverReplaces(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	if err := fsatomic.CreateFile(path, []byte("first"), 0o600); err != nil {
		t.Fatalf("create: %v", err)
	}
	err := fsatomic.CreateFile(path, []byte("second"), 0o600)
	if !errors.Is(err, fs.ErrExist) {
		t.Fatalf("second create = %v, want fs.ErrExist", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "first" {
		t.Fatalf("contents = %q, want the first write kept", got)
	}
	assertOnlyEntries(t, dir, "f")
}

// TestCreateFileRaceHasOneWinner: of callers racing to create one path, exactly one
// succeeds, the rest see fs.ErrExist, and the file is the winner's whole write.
func TestCreateFileRaceHasOneWinner(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	const n = 16
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = fsatomic.CreateFile(path, []byte(strings.Repeat(strconv.Itoa(i%10), 4096)), 0o600)
		}()
	}
	wg.Wait()
	winners := 0
	for i, err := range errs {
		switch {
		case err == nil:
			winners++
		case !errors.Is(err, fs.ErrExist):
			t.Errorf("caller %d: %v, want nil or fs.ErrExist", i, err)
		}
	}
	if winners != 1 {
		t.Fatalf("%d callers created the file, want exactly 1", winners)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4096 || strings.Count(string(got), string(got[0])) != len(got) {
		t.Fatalf("file is not one caller's whole write (%d bytes)", len(got))
	}
	assertOnlyEntries(t, dir, "f")
}

func assertOnlyEntries(t *testing.T, dir string, want ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("directory holds %v, want %v", got, want)
	}
}

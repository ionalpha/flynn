package sandbox

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestLocalDirReader(t *testing.T) {
	l, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"b.txt", "a.txt"} {
		if err := os.WriteFile(filepath.Join(l.Root(), name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()

	entries, err := l.ReadDir(ctx, ".")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if !slices.Equal(names, []string{"a.txt", "b.txt"}) {
		t.Errorf("ReadDir = %v, want sorted names", names)
	}
	if info, err := l.Stat(ctx, "a.txt"); err != nil || info.Size() != 1 {
		t.Errorf("Stat = %v, %v", info, err)
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	for _, tc := range []struct {
		name string
		ctx  context.Context
		path string
		want error
	}{
		{"cancelled", cancelled, ".", context.Canceled},
		{"escape", ctx, "../outside", ErrDenied},
		{"missing", ctx, "nope", fs.ErrNotExist},
	} {
		_, statErr := l.Stat(tc.ctx, tc.path)
		_, readErr := l.ReadDir(tc.ctx, tc.path)
		for _, err := range []error{statErr, readErr} {
			if !errors.Is(err, tc.want) {
				t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
			} else if strings.Contains(err.Error(), l.Root()) {
				t.Errorf("%s: error names the host path: %v", tc.name, err)
			}
		}
	}

	if _, err := l.ReadDir(ctx, "a.txt"); err == nil || strings.Contains(err.Error(), l.Root()) {
		t.Errorf("ReadDir of a file: err = %v", err)
	}
}

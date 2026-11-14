package acquire

import (
	"bytes"
	"errors"
	"path/filepath"
	"testing"
)

// failingClose is a file whose writes succeed and whose close fails, the shape of a
// disk that reports a full device only when the descriptor is flushed.
type failingClose struct {
	bytes.Buffer
	syncErr, closeErr error
}

func (f *failingClose) Sync() error  { return f.syncErr }
func (f *failingClose) Close() error { return f.closeErr }

func TestWriteFileFailsWhenCloseFails(t *testing.T) {
	closeFail := errors.New("close: no space left on device")
	tests := []struct {
		name string
		file *failingClose
	}{
		{"close fails", &failingClose{closeErr: closeFail}},
		{"sync fails", &failingClose{syncErr: errors.New("sync failed")}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			old := openOut
			openOut = func(string) (outFile, error) { return tc.file, nil }
			t.Cleanup(func() { openOut = old })
			_, err := writeFile(filepath.Join(t.TempDir(), "out"), bytes.NewReader([]byte("data")), 100)
			if err == nil {
				t.Fatal("a file that fails to sync or close must fail the extraction")
			}
		})
	}
	t.Run("close error is the one returned", func(t *testing.T) {
		old := openOut
		openOut = func(string) (outFile, error) { return &failingClose{closeErr: closeFail}, nil }
		t.Cleanup(func() { openOut = old })
		_, err := writeFile(filepath.Join(t.TempDir(), "out"), bytes.NewReader([]byte("data")), 100)
		if !errors.Is(err, closeFail) {
			t.Fatalf("got %v, want the close error", err)
		}
	})
}

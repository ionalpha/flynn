package ndjson

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

func TestKindNamesEveryRefusal(t *testing.T) {
	for _, tc := range []struct {
		k    Kind
		want string
	}{
		{KindOversize, "oversize"},
		{KindDepth, "depth"},
		{KindBatch, "batch"},
		{KindNotObject, "not_object"},
		{KindEncoding, "encoding"},
		{KindUnterminated, "unterminated"},
		{Kind(99), "kind(99)"},
	} {
		if got := tc.k.String(); got != tc.want {
			t.Errorf("Kind(%d).String() = %q, want %q", int(tc.k), got, tc.want)
		}
	}
}

func TestFrameErrorReportsKindAndTrueSize(t *testing.T) {
	err := error(&FrameError{Kind: KindOversize, Size: 5 << 20})
	if got, want := err.Error(), "ndjson: refused oversize line of 5242880 bytes"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
}

// shortWriter accepts one byte fewer than it is given and reports no error, which io.Writer
// forbids and a misbehaving descriptor wrapper can still do.
type shortWriter struct{ buf bytes.Buffer }

func (s *shortWriter) Write(p []byte) (int, error) {
	n := len(p) - 1
	s.buf.Write(p[:n])
	return n, nil
}

func TestWriterTreatsAShortWriteAsFailure(t *testing.T) {
	w := NewWriter(&shortWriter{})
	if err := w.WriteFrame(map[string]int{"a": 1}); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short write returned %v, want io.ErrShortWrite", err)
	}
	if err := w.WriteFrame(map[string]int{"a": 2}); !errors.Is(err, ErrWriterBroken) {
		t.Fatalf("write after a short write returned %v, want ErrWriterBroken", err)
	}
}

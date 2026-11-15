package ndjson

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// ErrWriterBroken is returned by every write after a failed one. A partial write may
// have left half a frame on the stream, so the reader is out of step and nothing may
// follow.
var ErrWriterBroken = errors.New("ndjson: writer is broken by an earlier failed write")

// Writer writes one JSON record per line.
type Writer struct {
	mu  sync.Mutex
	w   io.Writer
	err error
}

// NewWriter returns a Writer that emits frames to w. Pass the guard's frame writer:
// only frames reach the stream if every frame goes through here.
func NewWriter(w io.Writer) *Writer { return &Writer{w: w} }

// WriteFrame encodes v as one line. encoding/json escapes control characters in
// strings and compacts a [json.RawMessage], so the encoding holds no LF; it also
// escapes U+2028 and U+2029, so a consumer splitting on them still sees one record.
// The line and its LF go out in one Write under a lock, so concurrent frames never
// interleave.
func (w *Writer) WriteFrame(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("ndjson: encode frame: %w", err)
	}
	b = append(b, '\n')

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return ErrWriterBroken
	}
	n, err := w.w.Write(b)
	if err == nil && n < len(b) {
		err = io.ErrShortWrite
	}
	if err != nil {
		w.err = err
		return fmt.Errorf("ndjson: write frame: %w", err)
	}
	return nil
}

package ndjson

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// ErrWriterBroken is returned by every write after one failed. A write that failed part
// way may have left half a frame on the stream, so the stream is no longer in step with
// its reader and nothing further may follow it.
var ErrWriterBroken = errors.New("ndjson: writer is broken by an earlier failed write")

// Writer writes one JSON record per line.
type Writer struct {
	mu  sync.Mutex
	w   io.Writer
	err error
}

// NewWriter returns a Writer that emits frames to w. Give it the guard's frame writer
// and nothing else: the guarantee that only frames reach the stream holds only if every
// frame is written through here.
func NewWriter(w io.Writer) *Writer { return &Writer{w: w} }

// WriteFrame encodes v as one line. The encoding contains no LF, because encoding/json
// escapes control characters inside strings and compacts a [json.RawMessage], and it
// escapes U+2028 and U+2029 as well, so a consumer that splits on them anyway still sees
// one record. The line and its LF go out
// in a single Write call under a lock, so concurrent callers never interleave frames.
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

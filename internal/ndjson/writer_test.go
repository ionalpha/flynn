package ndjson

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
)

func TestWriterEscapesLineSeparators(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	if err := w.WriteFrame(map[string]string{"a": "x\u2028y\u2029z\nw\r"}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, raw := range []string{"\u2028", "\u2029", "\r"} {
		if strings.Contains(out, raw) {
			t.Errorf("frame contains a raw %U: %q", []rune(raw)[0], out)
		}
	}
	if strings.Count(out, "\n") != 1 || !strings.HasSuffix(out, "\n") {
		t.Errorf("frame is not exactly one LF-terminated line: %q", out)
	}
	// What went out reads back as one record.
	got := drain(t, strings.NewReader(out))
	if len(got) != 1 || got[0].kind != 0 {
		t.Errorf("frame did not read back as one record: %v", got)
	}
}

func TestWriterRawMessageCannotBreakALine(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	raw := []byte("{\"a\":\n\"\u2028\"}")
	if err := w.WriteFrame(rawJSON(raw)); err != nil {
		t.Fatal(err)
	}
	if strings.Count(buf.String(), "\n") != 1 || strings.Contains(buf.String(), "\u2028") {
		t.Fatalf("a raw message broke the framing: %q", buf.String())
	}
}

type rawJSON []byte

func (r rawJSON) MarshalJSON() ([]byte, error) { return r, nil }

func TestWriterConcurrentFramesDoNotInterleave(t *testing.T) {
	var buf lockedBuffer
	w := NewWriter(&buf)
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = w.WriteFrame(map[string]any{"n": i, "pad": strings.Repeat("p", 4096)})
		}()
	}
	wg.Wait()
	got := drain(t, bytes.NewReader(buf.bytes()))
	if len(got) != 50 {
		t.Fatalf("read back %d records, want 50", len(got))
	}
	for _, o := range got {
		if o.kind != 0 {
			t.Fatalf("an interleaved write produced a refused line: %v", o)
		}
	}
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	// Two halves with a yield between, so an unserialised caller would interleave.
	h := len(p) / 2
	l.b.Write(p[:h])
	l.mu.Unlock()
	l.mu.Lock()
	l.b.Write(p[h:])
	return len(p), nil
}

func (l *lockedBuffer) bytes() []byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]byte(nil), l.b.Bytes()...)
}

func TestWriterBreaksAfterAFailedWrite(t *testing.T) {
	w := NewWriter(failingWriter{})
	if err := w.WriteFrame(map[string]int{"a": 1}); err == nil {
		t.Fatal("a failed write returned nil")
	}
	if err := w.WriteFrame(map[string]int{"a": 2}); !errors.Is(err, ErrWriterBroken) {
		t.Fatalf("write after failure = %v, want ErrWriterBroken", err)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 3, io.ErrClosedPipe }

func TestWriterRefusesUnencodableFrame(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	if err := w.WriteFrame(make(chan int)); err == nil {
		t.Fatal("a channel encoded")
	}
	if buf.Len() != 0 {
		t.Fatalf("a refused frame still wrote %q", buf.String())
	}
	// A refused frame does not break the writer: nothing reached the stream.
	if err := w.WriteFrame(map[string]int{"a": 1}); err != nil {
		t.Fatalf("writer broken by a frame that never reached the stream: %v", err)
	}
}

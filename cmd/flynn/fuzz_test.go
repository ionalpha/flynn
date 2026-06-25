package main

import (
	"errors"
	"io"
	"strings"
	"testing"
)

// FuzzReadMessage throws arbitrary byte streams at the terminal message reader and
// asserts it never panics and always terminates, whatever control sequences, partial
// escapes, paste markers, or invalid UTF-8 it is fed. The one correctness invariant
// it locks: the bracketed-paste markers are control bytes the reader consumes, so
// they must never leak into a returned message (which would mean a paste was
// misparsed as literal text). A finite input guarantees termination: x/term reads to
// EOF, so the accumulation loop cannot spin forever.
func FuzzReadMessage(f *testing.F) {
	f.Add("hello world\r")
	f.Add("")
	f.Add(pasteStart + "a\rb\r" + pasteEnd + "\r")
	f.Add(pasteStart + "no end marker and EOF")
	f.Add("\x1b[200~partial escape \x1b[ and \x00 nul\r")
	f.Add("naïve unicode \xff\xfe bytes\r")

	f.Fuzz(func(t *testing.T, raw string) {
		// A single enormous line exercises x/term's superlinear per-key redraw, not
		// readMessage's logic; skip it so fuzzing explores structural variety instead
		// of grinding on one giant input.
		if len(raw) > 1<<12 {
			return
		}
		msg, err := readMessage(termOver(raw))
		if err != nil {
			// io.EOF or any read/parse error is a clean, expected outcome.
			return
		}
		if strings.Contains(msg, pasteStart) || strings.Contains(msg, pasteEnd) {
			t.Fatalf("bracketed-paste markers leaked into a message: %q", msg)
		}
		_ = errors.Is(err, io.EOF)
	})
}

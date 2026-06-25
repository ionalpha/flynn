package main

import (
	"errors"
	"io"
	"strings"
	"unicode/utf8"

	"golang.org/x/term"
)

// lineReader yields one user message per call and returns io.EOF when input ends.
// It is the seam between the session loop and however input is collected, so the
// loop is driven by a real terminal in production and by a scripted reader in tests
// without the loop knowing the difference.
type lineReader interface {
	ReadLine() (string, error)
}

// stdio adapts a separate reader and writer into the single io.ReadWriter that
// term.Terminal expects (keystrokes in, prompt and echo out).
type stdio struct {
	io.Reader
	io.Writer
}

// termReader reads a message from an interactive terminal with line editing, input
// history, and bracketed paste, via golang.org/x/term. It works the same on Linux,
// macOS, and Windows (x/term drives the platform's console mode directly).
//
// Raw mode is entered only for the duration of a read and restored before the call
// returns. That is deliberate: while a turn runs the terminal is in its normal mode,
// so Ctrl-C is delivered as SIGINT and cancels the turn (see runTurn); raw mode
// would instead swallow it as a keystroke. At the prompt, where raw mode is active,
// x/term reports Ctrl-C and Ctrl-D alike as io.EOF, which the loop treats as leave.
type termReader struct {
	fd   int
	term *term.Terminal
}

// newTermReader builds a terminal reader over rw (typically stdin+stdout) whose
// console file descriptor is fd, showing prompt before each line. Input is filtered
// to valid UTF-8 first (see utf8Reader): x/term discards an invalid-rune read
// without consuming it, so a long run of invalid bytes (a binary paste) would
// otherwise fill its input buffer and spin forever.
func newTermReader(rw io.ReadWriter, fd int, prompt string) *termReader {
	t := term.NewTerminal(stdio{&utf8Reader{src: rw}, rw}, prompt)
	t.SetBracketedPasteMode(true)
	return &termReader{fd: fd, term: t}
}

// utf8Reader passes valid UTF-8 through unchanged and drops bytes that are not, so
// the terminal line reader above it always makes forward progress. Every read either
// yields at least one valid byte or surfaces the underlying error once the buffer is
// drained: it never returns (0, nil) with bytes still pending, which is the state
// that hangs x/term. All ASCII (including control bytes and escape sequences) is
// valid UTF-8 and is preserved, so line editing and bracketed paste are unaffected.
type utf8Reader struct {
	src     io.Reader
	pending []byte
	srcErr  error
}

func (r *utf8Reader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		// x/term reads into a zero-length buffer only when its own input buffer is
		// full of bytes it can neither parse nor consume (a long run of un-terminated
		// escape bytes, say). Returning io.EOF here breaks that spin and ends the
		// read, rather than busy-looping forever. It never fires in normal use, where
		// term consumes tokens and always offers a non-empty buffer.
		return 0, io.EOF
	}
	for {
		out := 0
		for out < len(p) && len(r.pending) > 0 {
			b := r.pending[0]
			if b < utf8.RuneSelf { // ASCII: the common case, including ESC and CR
				p[out] = b
				out++
				r.pending = r.pending[1:]
				continue
			}
			if !utf8.FullRune(r.pending) {
				// A possibly-incomplete trailing multibyte sequence: wait for more
				// input to complete it, unless none is coming, in which case it is
				// junk and we drop one byte to keep moving.
				if r.srcErr == nil && len(r.pending) < utf8.UTFMax {
					break
				}
				r.pending = r.pending[1:]
				continue
			}
			rn, sz := utf8.DecodeRune(r.pending)
			if rn == utf8.RuneError && sz == 1 {
				r.pending = r.pending[1:] // invalid byte: drop it
				continue
			}
			if out+sz > len(p) {
				break // valid rune, but it does not fit this read; deliver it next time
			}
			copy(p[out:], r.pending[:sz])
			out += sz
			r.pending = r.pending[sz:]
		}
		if out > 0 {
			return out, nil
		}
		if r.srcErr != nil && len(r.pending) == 0 {
			return 0, r.srcErr
		}
		tmp := make([]byte, 1024)
		n, err := r.src.Read(tmp)
		r.pending = append(r.pending, tmp[:n]...)
		if err != nil {
			r.srcErr = err
		}
	}
}

// ReadLine returns the next message the user enters, or io.EOF when they leave.
func (r *termReader) ReadLine() (string, error) {
	old, err := term.MakeRaw(r.fd)
	if err != nil {
		return "", err
	}
	defer func() { _ = term.Restore(r.fd, old) }()
	return readMessage(r.term)
}

// Close disables bracketed paste, undoing the terminal mode the reader requested.
func (r *termReader) Close() { r.term.SetBracketedPasteMode(false) }

// readMessage reads one logical message from t. A bracketed paste that spans lines
// arrives from x/term one segment at a time, each flagged ErrPasteIndicator; this
// assembles those segments into a single message instead of firing a turn per pasted
// line. The message is sent when the user presses enter on a normally typed line,
// which is appended to any pending pasted segments, so a paste followed by typing
// sends as one message. It is separated from the raw-mode plumbing so the assembly
// can be tested against a real term.Terminal over an in-memory stream.
func readMessage(t *term.Terminal) (string, error) {
	var pasted strings.Builder
	for {
		line, err := t.ReadLine()
		switch {
		case errors.Is(err, term.ErrPasteIndicator):
			pasted.WriteString(line)
			pasted.WriteByte('\n')
		case errors.Is(err, io.EOF):
			if pasted.Len() > 0 {
				// A paste with no terminating typed line still counts as a message.
				return strings.TrimRight(pasted.String(), "\n"), nil
			}
			return "", io.EOF
		case err != nil:
			return "", err
		default:
			if pasted.Len() > 0 {
				return pasted.String() + line, nil
			}
			return line, nil
		}
	}
}

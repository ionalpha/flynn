package main

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"pgregory.net/rapid"

	"golang.org/x/term"
)

// The control sequences a terminal sends around a bracketed paste. x/term watches
// for exactly these to bracket pasted input.
const (
	pasteStart = "\x1b[200~"
	pasteEnd   = "\x1b[201~"
)

// termOver builds a term.Terminal that reads the given raw byte stream and discards
// its output, so readMessage can be exercised against the real x/term line reader
// without a tty. It wraps input in the same utf8Reader the production reader uses, so
// tests run the real path. Enter is carriage return, as a terminal sends it.
func termOver(input string) *term.Terminal {
	return termOverReader(strings.NewReader(input))
}

// termOverReader is termOver for an arbitrary byte source (a flaky or drip-feeding
// reader), wrapping it in the production utf8Reader filter.
func termOverReader(r io.Reader) *term.Terminal {
	return term.NewTerminal(stdio{&utf8Reader{src: r}, io.Discard}, "> ")
}

// TestReadMessageTypedLine: a normally typed line is returned as one message.
func TestReadMessageTypedLine(t *testing.T) {
	got, err := readMessage(termOver("hello world\r"))
	if err != nil {
		t.Fatalf("readMessage: %v", err)
	}
	if got != "hello world" {
		t.Fatalf("got %q, want %q", got, "hello world")
	}
}

// TestReadMessageEOF: an exhausted input reports io.EOF, which the loop reads as the
// user leaving (Ctrl-D, or Ctrl-C at the prompt).
func TestReadMessageEOF(t *testing.T) {
	if _, err := readMessage(termOver("")); !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want io.EOF", err)
	}
}

// TestReadMessageMultiLinePaste is the key paste invariant: a paste spanning lines,
// which x/term surfaces one segment at a time, is assembled into ONE message rather
// than firing a turn per pasted line. The trailing enter after the paste sends it.
func TestReadMessageMultiLinePaste(t *testing.T) {
	in := pasteStart + "line one\r" + "line two\r" + pasteEnd + "\r"
	got, err := readMessage(termOver(in))
	if err != nil {
		t.Fatalf("readMessage: %v", err)
	}
	for _, want := range []string{"line one", "line two"} {
		if !strings.Contains(got, want) {
			t.Fatalf("assembled message %q missing %q", got, want)
		}
	}
	if !strings.Contains(got, "\n") {
		t.Fatalf("multi-line paste collapsed to a single line: %q", got)
	}
}

// TestReadMessagePasteThenTyped: text typed after a paste is appended to it, so the
// whole thing is sent as one message when the user presses enter.
func TestReadMessagePasteThenTyped(t *testing.T) {
	in := pasteStart + "pasted\r" + pasteEnd + "and typed\r"
	got, err := readMessage(termOver(in))
	if err != nil {
		t.Fatalf("readMessage: %v", err)
	}
	if !strings.Contains(got, "pasted") || !strings.Contains(got, "and typed") {
		t.Fatalf("message %q did not merge the paste and the typed tail", got)
	}
}

// lineSpec is one expected message in the property test: either a typed line or a
// bracketed paste of one or more segments.
type lineSpec struct {
	paste bool
	texts []string
}

// TestReadMessageOneMessagePerEntryProperty is the input contract: over any
// sequence of typed lines and multi-line pastes, readMessage yields exactly one
// message per entry (a paste is one message, never one per pasted line), each
// message carries that entry's text, and the bracketed-paste control markers never
// leak into a message.
func TestReadMessageOneMessagePerEntryProperty(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(1, 6).Draw(rt, "entries")
		var specs []lineSpec
		var stream strings.Builder
		for i := range n {
			if rapid.Bool().Draw(rt, fmt.Sprintf("isPaste%d", i)) {
				k := rapid.IntRange(1, 3).Draw(rt, fmt.Sprintf("segs%d", i))
				segs := make([]string, k)
				stream.WriteString(pasteStart)
				for j := range segs {
					segs[j] = rapid.StringMatching(`[a-z]{1,6}`).Draw(rt, fmt.Sprintf("seg%d_%d", i, j))
					stream.WriteString(segs[j])
					stream.WriteString("\r")
				}
				stream.WriteString(pasteEnd)
				stream.WriteString("\r") // the enter that sends the paste
				specs = append(specs, lineSpec{paste: true, texts: segs})
			} else {
				txt := rapid.StringMatching(`[a-z]{1,8}`).Draw(rt, fmt.Sprintf("typed%d", i))
				stream.WriteString(txt)
				stream.WriteString("\r")
				specs = append(specs, lineSpec{texts: []string{txt}})
			}
		}

		tm := termOver(stream.String())
		var msgs []string
		for {
			m, err := readMessage(tm)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			msgs = append(msgs, m)
		}

		if len(msgs) != len(specs) {
			t.Fatalf("got %d messages, want %d (a paste must collapse to one message)\nstream=%q\nmsgs=%q",
				len(msgs), len(specs), stream.String(), msgs)
		}
		for i, sp := range specs {
			for _, txt := range sp.texts {
				if !strings.Contains(msgs[i], txt) {
					t.Fatalf("message %d = %q missing entry text %q", i, msgs[i], txt)
				}
			}
			if strings.Contains(msgs[i], pasteStart) || strings.Contains(msgs[i], pasteEnd) {
				t.Fatalf("message %d leaked paste markers: %q", i, msgs[i])
			}
		}
	})
}

// byteAtATimeReader delivers its data one byte per Read, modelling input that
// arrives fragmented across reads (keystrokes, a paste split over several reads).
type byteAtATimeReader struct {
	data []byte
	i    int
}

func (r *byteAtATimeReader) Read(p []byte) (int, error) {
	if r.i >= len(r.data) {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	p[0] = r.data[r.i]
	r.i++
	return 1, nil
}

// drain reads every message from a terminal until EOF.
func drain(t *testing.T, tm *term.Terminal) []string {
	t.Helper()
	var msgs []string
	for {
		m, err := readMessage(tm)
		if errors.Is(err, io.EOF) {
			return msgs
		}
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		msgs = append(msgs, m)
	}
}

// TestReadMessageDripFedMatchesBulk is the fragmentation-robustness property: feeding
// the same input one byte at a time (the worst-case read fragmentation a real
// terminal or a paste produces) yields exactly the same messages as feeding it all
// at once. This exercises x/term's partial-key remainder buffering through our
// reader.
func TestReadMessageDripFedMatchesBulk(t *testing.T) {
	stream := "first line\r" +
		pasteStart + "alpha\rbeta\rgamma\r" + pasteEnd + "\r" +
		"last line\r"

	bulk := drain(t, termOver(stream))
	drip := drain(t, termOverReader(&byteAtATimeReader{data: []byte(stream)}))

	if len(bulk) != len(drip) {
		t.Fatalf("drip-fed produced %d messages, bulk produced %d: %q vs %q", len(drip), len(bulk), drip, bulk)
	}
	for i := range bulk {
		if bulk[i] != drip[i] {
			t.Fatalf("message %d differs: bulk=%q drip=%q", i, bulk[i], drip[i])
		}
	}
}

// errAfterReader returns n bytes, then a fixed error, modelling a terminal read that
// fails mid-stream (a closed pty, an I/O error).
type errAfterReader struct {
	data []byte
	i    int
	stop int
	err  error
}

func (r *errAfterReader) Read(p []byte) (int, error) {
	if r.i >= r.stop || r.i >= len(r.data) {
		return 0, r.err
	}
	if len(p) == 0 {
		return 0, nil
	}
	p[0] = r.data[r.i]
	r.i++
	return 1, nil
}

// TestReadMessageSurfacesReadError is the input-fault property: a read failure
// mid-line is surfaced cleanly (returned, not panicked or hung), so the loop can end
// the session rather than spin.
func TestReadMessageSurfacesReadError(t *testing.T) {
	boom := errors.New("terminal read failed")
	r := &errAfterReader{data: []byte("half a li"), stop: 4, err: boom}
	_, err := readMessage(termOverReader(r))
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the injected read error", err)
	}
}

// TestReadMessageDoesNotHangOnBinary is the regression for the hangs fuzzing found:
// neither a long run of invalid UTF-8 (a binary paste) nor a long run of
// un-terminated escape bytes may spin forever. Invalid UTF-8 is dropped by the
// filter; an unparseable escape run trips the zero-length-read backstop. Either way
// the read returns promptly; the per-case timeout fails loudly if a hang returns.
func TestReadMessageDoesNotHangOnBinary(t *testing.T) {
	cases := map[string]string{
		"invalid_utf8": strings.Repeat("\xff", 4096) + "\r",
		"escape_run":   strings.Repeat("\x1b", 4096) + "\r",
		"escape_csi":   strings.Repeat("\x1b[", 2048) + "\r",
	}
	for name, junk := range cases {
		t.Run(name, func(t *testing.T) {
			done := make(chan struct{})
			go func() {
				defer close(done)
				_, _ = readMessage(termOver(junk)) // any return is fine; a hang is not
			}()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("readMessage hung on a long run of unparseable input (the x/term spin)")
			}
		})
	}
}

package screen_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/ionalpha/flynn/internal/testkit"
	"github.com/ionalpha/flynn/internal/tui/screen"
)

var errTerm = errors.New("terminal gone")

// TestChaosPainterDeadTerminal: a terminal that dies on the very first write
// (a closed pty) leaves the painter errored but inert: every later operation
// is a silent no-op, so session teardown never cascades secondary failures.
func TestChaosPainterDeadTerminal(t *testing.T) {
	w := testkit.FaultyWriter(nil, testkit.Always(errTerm))
	p := screen.NewPainter(w, 40, 10)
	p.Paint([]string{"one"})
	if !errors.Is(p.Err(), errTerm) {
		t.Fatalf("Err = %v, want the terminal error", p.Err())
	}
	p.Paint([]string{"two"})
	p.Insert([]string{"x"}, []string{"y"})
	p.Repaint([]string{"z"})
	p.Close()
	if !errors.Is(p.Err(), errTerm) {
		t.Fatalf("Err after later ops = %v, want the original error preserved", p.Err())
	}
}

// TestChaosPainterMidSessionFailure: a terminal that dies mid-session stops
// the painter at the failing frame; nothing further reaches the writer, so a
// dropped SSH connection cannot make the renderer spin on a dead stream.
func TestChaosPainterMidSessionFailure(t *testing.T) {
	var sink countingWriter
	w := testkit.FaultyWriter(&sink, testkit.FailOnCall(2, errTerm))
	p := screen.NewPainter(w, 40, 10)

	p.Paint([]string{"one"}) // call 1: succeeds
	if p.Err() != nil {
		t.Fatalf("healthy paint errored: %v", p.Err())
	}
	p.Paint([]string{"two"}) // call 2: the terminal dies
	if !errors.Is(p.Err(), errTerm) {
		t.Fatalf("Err = %v, want the injected failure", p.Err())
	}
	writesAtFailure := sink.writes
	p.Paint([]string{"three"})
	p.Insert([]string{"a"}, []string{"b"})
	p.Close()
	if sink.writes != writesAtFailure {
		t.Fatalf("%d writes reached the dead terminal after the failure", sink.writes-writesAtFailure)
	}
}

type countingWriter struct {
	writes int
	buf    bytes.Buffer
}

func (w *countingWriter) Write(p []byte) (int, error) {
	w.writes++
	return w.buf.Write(p)
}

// FuzzPainter drives the painter with operation sequences and frame content
// derived from arbitrary bytes and holds it to the same contract as the
// property test: a virtual terminal interpreting the output always shows the
// committed lines followed by the current frame, and the painter never
// panics or emits an escape sequence the terminal vocabulary does not
// contain (the vt panics on any unknown byte shape).
func FuzzPainter(f *testing.F) {
	f.Add([]byte("paint\xffline one\xfeline two\xffinsert\xfffinal"))
	f.Add([]byte("\xff\xff\xff"))
	f.Add([]byte("repaint\xffx"))

	const width, height = 20, 6
	f.Fuzz(func(t *testing.T, data []byte) {
		term := &vt{}
		var out bytes.Buffer
		p := screen.NewPainter(&out, width, height)
		var committed, frame []string

		for _, seg := range bytes.Split(data, []byte{0xff}) {
			parts := bytes.Split(seg, []byte{0xfe})
			op := len(seg) % 3
			lines := fuzzLines(parts, width, height-1)
			switch op {
			case 0:
				frame = lines
				p.Paint(frame)
			case 1:
				p.Insert(lines, frame)
				committed = append(committed, lines...)
			case 2:
				p.Repaint(frame)
			}
			if p.Err() != nil {
				t.Fatalf("painter error on an in-memory buffer: %v", p.Err())
			}
			term.write(out.String())
			out.Reset()
			want := trimTrailingBlank(append(append([]string{}, committed...), frame...))
			if got := term.screen(); !equal(got, want) {
				t.Fatalf("terminal diverged\n got %q\nwant %q", got, want)
			}
		}
	})
}

// fuzzLines shapes raw fuzz segments into lines the vt comparison can check
// exactly: printable ASCII, within the width, within the row cap.
func fuzzLines(parts [][]byte, width, maxRows int) []string {
	lines := make([]string, 0, len(parts))
	for _, part := range parts {
		var b []byte
		for _, c := range part {
			if c >= 0x20 && c <= 0x7e {
				b = append(b, c)
			}
			if len(b) == width {
				break
			}
		}
		lines = append(lines, string(b))
		if len(lines) == maxRows {
			break
		}
	}
	return lines
}

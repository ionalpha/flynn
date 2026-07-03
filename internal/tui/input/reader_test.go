package input_test

import (
	"io"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/internal/tui/input"
)

const escDelay = 5 * time.Millisecond

func awaitEvent(t *testing.T, ch <-chan input.Event) input.Event {
	t.Helper()
	select {
	case ev, open := <-ch:
		if !open {
			t.Fatal("event stream closed early")
		}
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("no event arrived")
		return nil
	}
}

func TestReaderResolvesLoneEscapeOnIdle(t *testing.T) {
	pr, pw := io.Pipe()
	r := input.NewReader(pr, clock.System{}, escDelay)
	defer func() { _ = pw.Close(); r.Stop() }()

	if _, err := pw.Write([]byte{0x1b}); err != nil {
		t.Fatal(err)
	}
	ev := awaitEvent(t, r.Events())
	if k, isKey := ev.(input.Key); !isKey || k.Code != input.KeyEsc {
		t.Fatalf("lone ESC decoded as %#v, want the Escape key", ev)
	}
}

func TestReaderKeepsACompleteSequenceWhole(t *testing.T) {
	pr, pw := io.Pipe()
	r := input.NewReader(pr, clock.System{}, escDelay)
	defer func() { _ = pw.Close(); r.Stop() }()

	if _, err := pw.Write([]byte("\x1b[A")); err != nil {
		t.Fatal(err)
	}
	ev := awaitEvent(t, r.Events())
	if k, isKey := ev.(input.Key); !isKey || k.Code != input.KeyUp {
		t.Fatalf("arrow decoded as %#v, want KeyUp", ev)
	}
}

func TestReaderNeverSplitsAStalledPaste(t *testing.T) {
	pr, pw := io.Pipe()
	r := input.NewReader(pr, clock.System{}, escDelay)
	defer func() { _ = pw.Close(); r.Stop() }()

	if _, err := pw.Write([]byte("\x1b[200~first half ")); err != nil {
		t.Fatal(err)
	}
	// Stall far past the escape delay mid-paste, then finish it.
	time.Sleep(10 * escDelay)
	if _, err := pw.Write([]byte("second half\x1b[201~")); err != nil {
		t.Fatal(err)
	}
	ev := awaitEvent(t, r.Events())
	p, isPaste := ev.(input.Paste)
	if !isPaste {
		t.Fatalf("stalled paste decoded as %#v, want one Paste", ev)
	}
	if p.Text != "first half second half" {
		t.Fatalf("paste text = %q, want the halves joined", p.Text)
	}
}

func TestReaderFlushesAndClosesOnEOF(t *testing.T) {
	pr, pw := io.Pipe()
	r := input.NewReader(pr, clock.System{}, escDelay)
	defer r.Stop()

	if _, err := pw.Write([]byte("a")); err != nil {
		t.Fatal(err)
	}
	if ev := awaitEvent(t, r.Events()); ev != (input.Key{Code: 'a'}) {
		t.Fatalf("got %#v, want the a key", ev)
	}
	_ = pw.Close()
	select {
	case _, open := <-r.Events():
		if open {
			t.Fatal("expected the stream to close after EOF")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not close on EOF")
	}
}

// gateReader is an input source the test controls read by read: entered
// signals each time a Read blocks, and data supplies its bytes (closing it
// is EOF). It makes the pause protocol deterministic: once entered fires,
// a read is in flight and a pause must take the poke path.
type gateReader struct {
	entered chan struct{}
	data    chan []byte
}

func newGateReader() *gateReader {
	return &gateReader{entered: make(chan struct{}, 8), data: make(chan []byte)}
}

func (g *gateReader) Read(p []byte) (int, error) {
	g.entered <- struct{}{}
	b, ok := <-g.data
	if !ok {
		return 0, io.EOF
	}
	return copy(p, b), nil
}

func (g *gateReader) awaitRead(t *testing.T) {
	t.Helper()
	select {
	case <-g.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("no read started")
	}
}

// TestReaderPauseParksPokesOnceAndResumes walks the whole pause protocol
// with a read in flight: the poke runs exactly once, its answer is
// discarded rather than decoded, the park is acknowledged, and after Resume
// the reader decodes fresh input as if nothing happened.
func TestReaderPauseParksPokesOnceAndResumes(t *testing.T) {
	g := newGateReader()
	r := input.NewReader(g, clock.System{}, escDelay)
	defer r.Stop()

	g.awaitRead(t) // a read is blocked; the token is spoken for
	pokes := 0
	parked := r.Pause(func() {
		pokes++
		go func() { g.data <- []byte("\x1b[10;1R") }() // the terminal answers the query
	})
	select {
	case <-parked:
	case <-time.After(2 * time.Second):
		t.Fatal("reader never parked")
	}
	if pokes != 1 {
		t.Fatalf("poke ran %d times, want 1", pokes)
	}
	select {
	case ev := <-r.Events():
		t.Fatalf("event %#v leaked through the pause", ev)
	case <-time.After(20 * time.Millisecond):
	}

	r.Resume()
	g.awaitRead(t)
	g.data <- []byte("b")
	if ev := awaitEvent(t, r.Events()); ev != (input.Key{Code: 'b'}) {
		t.Fatalf("after resume got %#v, want the b key", ev)
	}

	close(g.data) // EOF still closes the stream cleanly after a pause cycle
	select {
	case _, open := <-r.Events():
		if open {
			t.Fatal("expected the stream to close after EOF")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not close on EOF")
	}
}

// TestReaderResumeDropsAPartialSequence proves decoding restarts fresh: a
// partial escape sequence read just before the park never combines with
// input typed after the resume.
func TestReaderResumeDropsAPartialSequence(t *testing.T) {
	g := newGateReader()
	r := input.NewReader(g, clock.System{}, escDelay)
	defer r.Stop()

	g.awaitRead(t)
	parked := r.Pause(func() {
		go func() { g.data <- []byte("\x1b[") }() // half a sequence answers the poke
	})
	select {
	case <-parked:
	case <-time.After(2 * time.Second):
		t.Fatal("reader never parked")
	}
	r.Resume()
	g.awaitRead(t)
	g.data <- []byte("A") // would complete the dropped half into KeyUp
	if ev := awaitEvent(t, r.Events()); ev != (input.Key{Code: 'A'}) {
		t.Fatalf("got %#v, want a plain A key, not an arrow", ev)
	}
	close(g.data)
}

func TestReaderStopReleasesThePump(t *testing.T) {
	pr, pw := io.Pipe()
	r := input.NewReader(pr, clock.System{}, escDelay)
	done := make(chan struct{})
	go func() {
		r.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop hung")
	}
	_ = pw.Close()
}

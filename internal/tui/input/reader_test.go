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

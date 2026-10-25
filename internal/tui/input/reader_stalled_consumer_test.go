package input_test

import (
	"bytes"
	"io"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/internal/tui/input"
)

// eventsCap is the reader's event buffer; a consumer that stops reading fills it
// and the pump then blocks inside emit.
const eventsCap = 64

// awaitFull waits until the reader's event buffer is full, which means the pump
// is (or is about to be) blocked delivering the next event.
func awaitFull(t *testing.T, r *input.Reader) {
	t.Helper()
	timeout := time.After(2 * time.Second)
	for len(r.Events()) < eventsCap {
		select {
		case <-timeout:
			t.Fatalf("event buffer never filled: %d of %d", len(r.Events()), eventsCap)
		case <-time.After(time.Millisecond):
		}
	}
}

func requireStops(t *testing.T, r *input.Reader) {
	t.Helper()
	stopped := make(chan struct{})
	go func() {
		r.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop hung on a pump blocked delivering events")
	}
}

// TestReaderStopReleasesAPumpBlockedOnAFullBuffer checks that a consumer which has
// gone away cannot wedge the pump: a chunk that decodes to more events than the
// buffer holds leaves the pump blocked in emit, and Stop must still release it.
func TestReaderStopReleasesAPumpBlockedOnAFullBuffer(t *testing.T) {
	pr, pw := io.Pipe()
	r := input.NewReader(pr, clock.System{}, escDelay)
	defer func() { _ = pw.Close() }()

	if _, err := pw.Write(bytes.Repeat([]byte("a"), 2*eventsCap)); err != nil {
		t.Fatal(err)
	}
	awaitFull(t, r)

	requireStops(t, r)
}

// TestReaderStopReleasesAnIdleFlushBlockedOnAFullBuffer is the same guarantee for
// the idle flush: the Escape it resolves has nowhere to go once the buffer is full.
func TestReaderStopReleasesAnIdleFlushBlockedOnAFullBuffer(t *testing.T) {
	c := newArmClock()
	pr, pw := io.Pipe()
	r := input.NewReader(pr, c, escDelay)
	defer func() { _ = pw.Close() }()

	// A full buffer of keys, then a lone Escape the decoder holds back as a
	// possible sequence start.
	if _, err := pw.Write(append(bytes.Repeat([]byte("a"), eventsCap), 0x1b)); err != nil {
		t.Fatal(err)
	}
	awaitFull(t, r)
	c.awaitArmed(t)
	time.Sleep(20 * time.Millisecond) // let the pump park in its select before the timer fires
	c.Advance(escDelay)

	requireStops(t, r)
}

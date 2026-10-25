package input_test

import (
	"io"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/internal/tui/input"
)

// armClock is a Manual clock that reports each timer the reader arms, so a
// test knows the idle timer exists before it advances time.
type armClock struct {
	*clock.Manual
	armed chan struct{}
}

func newArmClock() *armClock {
	return &armClock{Manual: clock.NewManual(time.Unix(0, 0)), armed: make(chan struct{}, 8)}
}

func (c *armClock) NewTimer(d time.Duration) clock.Timer {
	t := c.Manual.NewTimer(d)
	c.armed <- struct{}{}
	return t
}

func (c *armClock) awaitArmed(t *testing.T) {
	t.Helper()
	select {
	case <-c.armed:
	case <-time.After(2 * time.Second):
		t.Fatal("idle timer was never armed")
	}
}

func (c *armClock) requireNoEvent(t *testing.T, ch <-chan input.Event) {
	t.Helper()
	select {
	case ev := <-ch:
		t.Fatalf("unexpected event %#v", ev)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestReaderIdleFlushWaitsForTheClock(t *testing.T) {
	c := newArmClock()
	pr, pw := io.Pipe()
	r := input.NewReader(pr, c, escDelay)
	defer func() { _ = pw.Close(); r.Stop() }()

	if _, err := pw.Write([]byte{0x1b}); err != nil {
		t.Fatal(err)
	}
	c.awaitArmed(t)
	c.requireNoEvent(t, r.Events())
	c.Advance(escDelay)
	if k, isKey := awaitEvent(t, r.Events()).(input.Key); !isKey || k.Code != input.KeyEsc {
		t.Fatalf("after the delay got %#v, want the Escape key", k)
	}
}

func TestReaderLaterChunkCancelsTheIdleFlush(t *testing.T) {
	c := newArmClock()
	pr, pw := io.Pipe()
	r := input.NewReader(pr, c, escDelay)
	defer func() { _ = pw.Close(); r.Stop() }()

	if _, err := pw.Write([]byte{0x1b}); err != nil {
		t.Fatal(err)
	}
	c.awaitArmed(t)
	if _, err := pw.Write([]byte("[A")); err != nil {
		t.Fatal(err)
	}
	if k, isKey := awaitEvent(t, r.Events()).(input.Key); !isKey || k.Code != input.KeyUp {
		t.Fatalf("got %#v, want the completed arrow", k)
	}
	c.Advance(10 * escDelay)
	c.requireNoEvent(t, r.Events())
}

func TestReaderDoesNotArmTheTimerMidPaste(t *testing.T) {
	c := newArmClock()
	pr, pw := io.Pipe()
	r := input.NewReader(pr, c, escDelay)
	defer func() { _ = pw.Close(); r.Stop() }()

	if _, err := pw.Write([]byte("\x1b[200~half")); err != nil {
		t.Fatal(err)
	}
	c.requireNoEvent(t, r.Events())
	select {
	case <-c.armed:
		t.Fatal("idle timer armed mid-paste")
	default:
	}
}

func TestReaderStopReleasesAPendingPause(t *testing.T) {
	g := newGateReader()
	r := input.NewReader(g, clock.System{}, escDelay)

	g.awaitRead(t)
	parked := r.Pause(func() {}) // the poke never gets an answer
	// Stop returns only once the pump has exited, and the pump closes the
	// pending acknowledgement on its way out, so a waiter is never stranded.
	r.Stop()
	select {
	case <-parked:
	case <-time.After(2 * time.Second):
		t.Fatal("pause acknowledgement stranded after Stop")
	}
	close(g.data)
}

func TestReaderStopWithIdleTimerArmed(t *testing.T) {
	c := newArmClock()
	pr, pw := io.Pipe()
	r := input.NewReader(pr, c, escDelay)
	defer func() { _ = pw.Close() }()

	if _, err := pw.Write([]byte{0x1b}); err != nil {
		t.Fatal(err)
	}
	c.awaitArmed(t)
	r.Stop()
}

package input

import (
	"io"
	"time"

	"github.com/ionalpha/flynn/clock"
)

// Reader pumps a raw terminal byte stream through a Decoder and delivers
// events on a channel. It supplies the one policy the pure decoder cannot: a
// lone Escape byte is indistinguishable from the start of an escape sequence
// until more bytes arrive or enough time passes, so when a read leaves the
// decoder holding a partial sequence, the reader arms an idle timer and
// flushes if nothing follows. The timer never arms mid-paste: a large paste
// may stall between chunks, and flushing there would split it in two.
//
// Time comes from the injected clock.Timing, so the escape delay is testable
// without real sleeps and consistent with the runtime's determinism rules.
type Reader struct {
	events chan Event
	stop   chan struct{}
	done   chan struct{}
}

// NewReader starts pumping r. escDelay is how long a partial sequence may
// dangle before it is flushed (traditional terminals use tens of
// milliseconds; too low misreads fast sequences on slow links as Escape
// presses, too high makes the Escape key feel laggy).
func NewReader(r io.Reader, timing clock.Timing, escDelay time.Duration) *Reader {
	rd := &Reader{
		events: make(chan Event, 64),
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	chunks := make(chan []byte)
	go rd.read(r, chunks)
	go rd.pump(timing, escDelay, chunks)
	return rd
}

// Events is the decoded event stream. It closes when the input reaches EOF
// or errors, after a final flush, so a consumer ranges over it and exits
// cleanly with the terminal.
func (r *Reader) Events() <-chan Event { return r.events }

// Stop abandons the pump and waits for it to exit. The underlying read
// goroutine may stay blocked in its current Read until the input closes;
// that is inherent to blocking terminal reads, and callers close or restore
// the terminal right after Stop, which unblocks it.
func (r *Reader) Stop() {
	close(r.stop)
	<-r.done
}

// read moves raw chunks from the input to the pump. Each chunk is its own
// allocation because the pump consumes it asynchronously; sizing follows the
// largest burst a terminal delivers at once (a paste arrives in chunks of a
// few kilobytes).
func (r *Reader) read(src io.Reader, chunks chan<- []byte) {
	defer close(chunks)
	for {
		buf := make([]byte, 4096)
		n, err := src.Read(buf)
		if n > 0 {
			select {
			case chunks <- buf[:n]:
			case <-r.stop:
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// pump feeds the decoder and manages the idle flush.
func (r *Reader) pump(timing clock.Timing, escDelay time.Duration, chunks <-chan []byte) {
	defer close(r.done)
	var d Decoder
	var idle clock.Timer
	stopIdle := func() {
		if idle != nil {
			idle.Stop()
			idle = nil
		}
	}
	for {
		var idleC <-chan time.Time
		if idle != nil {
			idleC = idle.C()
		}
		select {
		case <-r.stop:
			stopIdle()
			return
		case chunk, open := <-chunks:
			stopIdle()
			if !open {
				r.emit(d.Flush())
				close(r.events)
				return
			}
			if !r.emit(d.Feed(chunk)) {
				return
			}
			if d.Pending() && !d.MidPaste() {
				idle = timing.NewTimer(escDelay)
			}
		case <-idleC:
			idle = nil
			if !r.emit(d.Flush()) {
				return
			}
		}
	}
}

// emit delivers events, honoring stop so a departed consumer never wedges
// the pump. It reports whether pumping should continue.
func (r *Reader) emit(evs []Event) bool {
	for _, ev := range evs {
		select {
		case r.events <- ev:
		case <-r.stop:
			return false
		}
	}
	return true
}

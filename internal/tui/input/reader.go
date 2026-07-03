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
// The reader can also be paused, parking its read goroutine off the input
// stream so another process (an external editor) can own the terminal
// exclusively, then resumed. Reads are token-gated to make that possible: the
// pump issues one token per read, so pausing withholds the next token instead
// of racing a blocked read for the stream's bytes.
//
// Time comes from the injected clock.Timing, so the escape delay is testable
// without real sleeps and consistent with the runtime's determinism rules.
type Reader struct {
	events chan Event
	stop   chan struct{}
	done   chan struct{}
	token  chan struct{}
	pause  chan pauseReq
	resume chan struct{}
}

// pauseReq asks the pump to park the reader. done closes once no read is in
// flight; poke runs when a read is already blocked, so the caller can elicit
// bytes that complete it.
type pauseReq struct {
	poke func()
	done chan struct{}
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
		token:  make(chan struct{}, 1),
		pause:  make(chan pauseReq, 1),
		resume: make(chan struct{}, 1),
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

// Pause parks the reader off its input and returns a channel that closes
// once no read is in flight, so the caller can hand the stream to another
// process without the two racing for its bytes. When a read is already
// blocked, poke runs exactly once; the caller uses it to elicit bytes on the
// stream (a terminal answers a cursor position query) that complete the
// read. Bytes read between Pause and the park are discarded, as is the
// poke's answer. The pump handles the request at its next iteration, so a
// caller waiting on the returned channel must keep draining Events until it
// closes. Pause and Resume alternate strictly, from one goroutine.
func (r *Reader) Pause(poke func()) <-chan struct{} {
	req := pauseReq{poke: poke, done: make(chan struct{})}
	select {
	case r.pause <- req:
	case <-r.done:
		close(req.done)
	}
	return req.done
}

// Resume lifts a pause: the pump issues the next read token and decoding
// starts fresh, dropping any partial escape sequence from before the pause.
func (r *Reader) Resume() {
	select {
	case r.resume <- struct{}{}:
	case <-r.done:
	}
}

// read moves raw chunks from the input to the pump, one token-gated read at
// a time. Each chunk is its own allocation because the pump consumes it
// asynchronously; sizing follows the largest burst a terminal delivers at
// once (a paste arrives in chunks of a few kilobytes).
func (r *Reader) read(src io.Reader, chunks chan<- []byte) {
	defer close(chunks)
	for {
		select {
		case <-r.token:
		case <-r.stop:
			return
		}
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

// pump feeds the decoder and manages the idle flush, the read tokens, and
// the pause protocol. Exactly one token is outstanding whenever the reader
// is running; a pause withholds the next one, reclaiming an unclaimed token
// when it can and otherwise poking the blocked read and waiting for its
// final chunk.
func (r *Reader) pump(timing clock.Timing, escDelay time.Duration, chunks <-chan []byte) {
	var pauseDone chan struct{}
	defer func() {
		if pauseDone != nil {
			close(pauseDone)
		}
		close(r.done)
	}()
	var d Decoder
	var idle clock.Timer
	paused := false
	outstanding := false
	stopIdle := func() {
		if idle != nil {
			idle.Stop()
			idle = nil
		}
	}
	for {
		if !paused && !outstanding {
			r.token <- struct{}{} // buffered; never blocks with one token in play
			outstanding = true
		}
		var idleC <-chan time.Time
		if idle != nil {
			idleC = idle.C()
		}
		select {
		case <-r.stop:
			stopIdle()
			return
		case req := <-r.pause:
			paused = true
			stopIdle()
			// Reclaim the token when the read goroutine has not taken it
			// yet: no read will start, so the park is immediate. Otherwise a
			// read is blocked; poke it and park when its chunk arrives.
			select {
			case <-r.token:
				outstanding = false
			default:
			}
			if !outstanding {
				close(req.done)
			} else {
				pauseDone = req.done
				req.poke()
			}
		case <-r.resume:
			paused = false
			d = Decoder{}
		case chunk, open := <-chunks:
			stopIdle()
			outstanding = false
			if pauseDone != nil {
				close(pauseDone)
				pauseDone = nil
			}
			if !open {
				r.emit(d.Flush())
				close(r.events)
				return
			}
			if paused {
				continue // raced the pause or answered the poke; not input
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

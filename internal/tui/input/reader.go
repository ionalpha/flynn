package input

import (
	"io"
	"time"

	"github.com/ionalpha/flynn/clock"
)

// Reader pumps a raw terminal byte stream through a Decoder and delivers events
// on a channel. A partial sequence arms an idle timer (from the injected
// clock.Timing) that flushes it, resolving a lone Escape; the timer never arms
// mid-paste, or a stall between chunks would split the paste. Reads are
// token-gated so a pause withholds the next token instead of racing a blocked
// read for the stream's bytes.
type Reader struct {
	events chan Event
	stop   chan struct{}
	done   chan struct{}
	token  chan struct{}
	pause  chan pauseReq
	resume chan struct{}
}

// pauseReq asks the pump to park the reader. done closes once no read is in
// flight; poke runs when a read is blocked, to elicit bytes that complete it.
type pauseReq struct {
	poke func()
	done chan struct{}
}

// NewReader starts pumping r. escDelay is how long a partial sequence may dangle
// before it is flushed: too low misreads sequences on slow links as Escape
// presses, too high makes Escape laggy. Tens of milliseconds is usual.
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

// Events is the decoded event stream. It closes after a final flush when the
// input reaches EOF or errors.
func (r *Reader) Events() <-chan Event { return r.events }

// Stop abandons the pump and waits for it to exit. The read goroutine may stay
// blocked in Read until the input closes, so callers close or restore the
// terminal right after.
func (r *Reader) Stop() {
	close(r.stop)
	<-r.done
}

// Pause parks the reader off its input and returns a channel that closes once no
// read is in flight, so the stream can go to another process without a race for
// its bytes. When a read is blocked, poke runs once to elicit bytes that
// complete it (a cursor position query). Bytes read between Pause and the park,
// and the poke's answer, are discarded. The pump handles the request at its next
// iteration, so a caller waiting on the channel must keep draining Events. Pause
// and Resume alternate strictly, from one goroutine.
func (r *Reader) Pause(poke func()) <-chan struct{} {
	req := pauseReq{poke: poke, done: make(chan struct{})}
	select {
	case r.pause <- req:
	case <-r.done:
		close(req.done)
	}
	return req.done
}

// Resume lifts a pause. Decoding starts fresh, dropping any partial sequence
// from before it.
func (r *Reader) Resume() {
	select {
	case r.resume <- struct{}{}:
	case <-r.done:
	}
}

// read moves raw chunks to the pump, one token-gated read at a time. Each chunk
// is its own allocation because the pump consumes it asynchronously.
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

// pump feeds the decoder and manages the idle flush, read tokens and pause
// protocol. Exactly one token is outstanding while running; a pause withholds the
// next, reclaiming an unclaimed token or else poking the blocked read and
// waiting for its final chunk.
func (r *Reader) pump(timing clock.Timing, escDelay time.Duration, chunks <-chan []byte) {
	p := &pumpState{r: r, timing: timing, escDelay: escDelay}
	defer p.finish()
	for {
		p.issueToken()
		select {
		case <-r.stop:
			p.stopIdle()
			return
		case req := <-r.pause:
			p.park(req)
		case <-r.resume:
			p.paused = false
			p.d = Decoder{}
		case chunk, open := <-chunks:
			if !p.chunk(chunk, open) {
				return
			}
		case <-p.idleC():
			if !p.flushIdle() {
				return
			}
		}
	}
}

// pumpState is the pump's loop-local state. outstanding is true while a read
// token is in play; pauseDone is owed to a Pause whose read is still blocked.
type pumpState struct {
	r           *Reader
	timing      clock.Timing
	escDelay    time.Duration
	d           Decoder
	idle        clock.Timer
	paused      bool
	outstanding bool
	pauseDone   chan struct{}
}

// finish releases a Pause still waiting on a read, so its caller is not
// stranded by an exiting pump, then marks the reader done.
func (p *pumpState) finish() {
	if p.pauseDone != nil {
		close(p.pauseDone)
	}
	close(p.r.done)
}

func (p *pumpState) issueToken() {
	if p.paused || p.outstanding {
		return
	}
	p.r.token <- struct{}{} // buffered; never blocks with one token in play
	p.outstanding = true
}

func (p *pumpState) stopIdle() {
	if p.idle != nil {
		p.idle.Stop()
		p.idle = nil
	}
}

// idleC is the idle timer's channel, or nil (which blocks forever in a select)
// when no timer is armed.
func (p *pumpState) idleC() <-chan time.Time {
	if p.idle == nil {
		return nil
	}
	return p.idle.C()
}

// park handles a pause request. An unclaimed token is reclaimed and the park is
// immediate; otherwise a read is blocked, so poke it and park when its chunk
// arrives.
func (p *pumpState) park(req pauseReq) {
	p.paused = true
	p.stopIdle()
	select {
	case <-p.r.token:
		p.outstanding = false
	default:
	}
	if !p.outstanding {
		close(req.done)
		return
	}
	p.pauseDone = req.done
	req.poke()
}

func (p *pumpState) chunk(chunk []byte, open bool) bool {
	p.stopIdle()
	p.outstanding = false
	if p.pauseDone != nil {
		close(p.pauseDone)
		p.pauseDone = nil
	}
	if !open {
		p.r.emit(p.d.Flush())
		close(p.r.events)
		return false
	}
	if p.paused {
		return true // raced the pause or answered the poke; not input
	}
	if !p.r.emit(p.d.Feed(chunk)) {
		return false
	}
	if p.d.Pending() && !p.d.MidPaste() {
		p.idle = p.timing.NewTimer(p.escDelay)
	}
	return true
}

func (p *pumpState) flushIdle() bool {
	p.idle = nil
	return p.r.emit(p.d.Flush())
}

// emit delivers events, honoring stop so a departed consumer cannot wedge the
// pump. It reports whether pumping should continue.
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

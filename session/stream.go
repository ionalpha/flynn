package session

import (
	"context"
	"time"

	"github.com/ionalpha/flynn/bus"
	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/spine"
)

// DefaultStreamPoll is how often a subscriber re-reads the spine without a bus
// wake, so a dropped or coalesced wake, or a missing bus, costs at most this
// much latency.
const DefaultStreamPoll = 250 * time.Millisecond

// Stream is an ordered, replayable event channel for one session. Each event is
// recorded on a spine stream, which assigns its Seq, and a wake is published on
// the bus. A subscriber gets the backlog and then every later event, in Seq
// order, without loss or duplication.
//
// Subscribers never read content off the bus. A wake or the poll floor re-reads
// everything past the subscriber's cursor from the spine, so the tail stays
// correct on an at-most-once bus and progresses on a bus that delivers nothing.
type Stream struct {
	log     spine.Log
	bus     bus.Bus
	stream  string
	subject string
	poll    time.Duration
	clk     clock.Timing
}

// newStream builds a Stream for session id. The id names the spine stream and
// suffixes the bus subject, so concurrent sessions stay isolated.
func newStream(log spine.Log, b bus.Bus, id string) *Stream {
	return &Stream{log: log, bus: b, stream: id, subject: subjectPrefix + id, poll: DefaultStreamPoll, clk: clock.System{}}
}

// subjectPrefix puts every session's wake under one subject hierarchy, so a
// host can subscribe to "session.events.>" for fleet-wide fan-out.
const subjectPrefix = "session.events."

// append records ev on the spine, then wakes subscribers. A publish error does
// not fail the append, because subscribers read the spine and the next wake
// covers a missed one.
func (s *Stream) append(ctx context.Context, ev Event) error {
	if _, err := s.log.Append(ctx, ev.toAppend(s.stream)); err != nil {
		return err
	}
	_ = s.bus.Publish(ctx, bus.Message{Subject: s.subject})
	return nil
}

// Subscribe returns a channel of every event after afterSeq: the backlog, then
// events as they arrive. Pass 0 to replay from the start. The channel closes
// when ctx is cancelled or the bus subscription ends; the caller drains it.
//
// One advancing cursor gives order and exactly-once delivery: each wake drains
// the spine from the cursor and advances it past every event sent, so a burst
// of wakes collapses into one read. A slow reader blocks only its own drain.
func (s *Stream) Subscribe(ctx context.Context, afterSeq int64) (<-chan Event, error) {
	out := make(chan Event)
	wake := make(chan struct{}, 1)

	sub, err := s.bus.Subscribe(ctx, s.subject, func(context.Context, bus.Message) error {
		// A full buffer already means "drain", and the drain reads everything past the
		// cursor, so dropping this wake loses nothing.
		select {
		case wake <- struct{}{}:
		default:
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	go func() {
		defer close(out)
		defer func() { _ = sub.Unsubscribe() }()

		t := &tail{s: s, out: out, cursor: afterSeq}
		// The bus subscription is live before this drain, so an event appended after
		// the read still wakes the tail.
		if !t.drain(ctx) {
			return
		}
		for s.await(ctx, wake) {
			if !t.drain(ctx) {
				return
			}
		}
	}()

	return out, nil
}

// tail is one subscriber's position. Its cursor gives ordered, exactly-once
// delivery.
type tail struct {
	s      *Stream
	out    chan<- Event
	cursor int64
}

// drain sends every event past the cursor, advancing it, until the spine has
// nothing newer. It reports false when the read failed or ctx was cancelled
// mid-send.
func (t *tail) drain(ctx context.Context) bool {
	for {
		evs, err := t.s.log.Read(ctx, spine.Query{Stream: t.s.stream, AfterSeq: t.cursor})
		if err != nil || len(evs) == 0 {
			return err == nil
		}
		for _, se := range evs {
			select {
			case t.out <- fromSpine(se):
			case <-ctx.Done():
				return false
			}
			t.cursor = se.Seq
		}
	}
}

// await blocks until a wake or the poll floor fires, and reports false once ctx
// is cancelled. A non-positive poll disables the floor (tests of the bus path).
// The timer is armed per wait and stopped when another case wins, so a Manual
// clock accumulates no stale timers.
func (s *Stream) await(ctx context.Context, wake <-chan struct{}) bool {
	var tick <-chan time.Time
	var timer clock.Timer
	if s.poll > 0 {
		timer = s.clk.NewTimer(s.poll)
		tick = timer.C()
	}
	stop := func() {
		if timer != nil {
			timer.Stop()
		}
	}
	select {
	case <-ctx.Done():
		stop()
		return false
	case <-wake:
		stop()
		return true
	case <-tick:
		return true
	}
}

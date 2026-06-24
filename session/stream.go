package session

import (
	"context"
	"time"

	"github.com/ionalpha/flynn/bus"
	"github.com/ionalpha/flynn/spine"
)

// DefaultStreamPoll is a subscriber's fallback re-read interval: how often it drains the
// spine even if no bus wake arrived. The bus delivers events promptly; this is the
// always-correct floor beneath it, so a coalesced, dropped, or never-delivered
// wake (a degraded or absent bus) costs at most this much latency rather than
// stalling the tail.
const DefaultStreamPoll = 250 * time.Millisecond

// Stream is an ordered, replayable, fan-out event channel for one session. Each
// appended event is recorded on a durable spine stream (which assigns its
// monotonic Seq) and a wake is published on the bus. A subscriber receives the
// full backlog followed by every later event, in Seq order, with no loss and no
// duplication.
//
// The split of roles is deliberate and mirrors the substrate: the spine is the
// durable, ordered source of truth, and the bus carries only a liveness signal.
// Subscribers never read content off the bus, so a coalesced or dropped wake costs
// nothing: the next wake (or the poll floor) re-reads everything past the
// subscriber's cursor from the spine. That makes the live tail correct even though
// the bus is at-most-once, and keeps it making progress even if the bus delivers
// nothing at all.
type Stream struct {
	log     spine.Log
	bus     bus.Bus
	stream  string
	subject string
	poll    time.Duration
}

// newStream builds a Stream over log and b for the session id. The id names both
// the spine stream and (as a suffix) the bus subject, so concurrent sessions on
// shared infrastructure stay isolated.
func newStream(log spine.Log, b bus.Bus, id string) *Stream {
	return &Stream{log: log, bus: b, stream: id, subject: subjectPrefix + id, poll: DefaultStreamPoll}
}

// subjectPrefix namespaces every session's wake subject under one bus hierarchy,
// so a host can subscribe to "session.events.>" for fleet-wide fan-out.
const subjectPrefix = "session.events."

// append records ev on the spine, then wakes live subscribers. The wake is best
// effort: a publish error never fails the append, because the durable record is
// what subscribers ultimately read and a missed wake self-heals on the next one.
func (s *Stream) append(ctx context.Context, ev Event) error {
	if _, err := s.log.Append(ctx, ev.toAppend(s.stream)); err != nil {
		return err
	}
	_ = s.bus.Publish(ctx, bus.Message{Subject: s.subject})
	return nil
}

// Subscribe returns a channel of every event after afterSeq, beginning with the
// backlog already on the spine and continuing with events as they arrive. Pass 0
// to replay the session from the start. The channel is closed when ctx is
// cancelled or the underlying bus subscription ends; the caller owns draining it.
//
// Ordering and exactly-once delivery come from a single advancing cursor: each
// wake drains the spine from the cursor forward and advances it past every event
// emitted, so no event is sent twice and a burst of wakes collapses into one read.
// Backpressure is per-subscriber: a slow reader blocks only its own drain, never
// the bus or other subscribers.
func (s *Stream) Subscribe(ctx context.Context, afterSeq int64) (<-chan Event, error) {
	out := make(chan Event)
	wake := make(chan struct{}, 1)

	sub, err := s.bus.Subscribe(ctx, s.subject, func(context.Context, bus.Message) error {
		// Coalesce: a full buffer already means "there is work to drain", and the
		// drain reads everything past the cursor regardless, so dropping the extra
		// signal loses nothing.
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

		// The poll floor guarantees progress even if no wake ever arrives; the bus
		// only makes delivery prompt. A non-positive poll disables the floor (used
		// in tests that want to prove the bus path alone).
		var tick <-chan time.Time
		if s.poll > 0 {
			t := time.NewTicker(s.poll)
			defer t.Stop()
			tick = t.C
		}

		cursor := afterSeq
		drain := func() bool {
			for {
				evs, err := s.log.Read(ctx, spine.Query{Stream: s.stream, AfterSeq: cursor})
				if err != nil || len(evs) == 0 {
					return err == nil
				}
				for _, se := range evs {
					select {
					case out <- fromSpine(se):
					case <-ctx.Done():
						return false
					}
					cursor = se.Seq
				}
			}
		}

		// The bus subscription is live before this first drain, so any event
		// appended after the read snapshot still wakes us and is not missed.
		if !drain() {
			return
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-wake:
				if !drain() {
					return
				}
			case <-tick:
				if !drain() {
					return
				}
			}
		}
	}()

	return out, nil
}

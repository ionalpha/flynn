package bus

import (
	"context"
	"sync"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/observe"
)

// defaultBuffer is the per-subscription mailbox depth. A slow handler applies
// backpressure to Publish once its mailbox fills, rather than growing unbounded;
// this keeps the in-process bus's memory bounded (the resource-hygiene rule)
// while preserving every message a handler can keep up with.
const defaultBuffer = 64

// MemoryBus is the zero-dependency, in-process Bus: pure Go channels, no broker,
// no network. It is the standalone default so the agent has working pub/sub with
// no setup, and the reference semantics the NATS adapter must match (held to
// bustest.RunSuite).
//
// Delivery is asynchronous and ordered per subscription: each subscription owns a
// goroutine that runs its handler over a buffered mailbox in publish order. A
// handler that panics or errors cannot take down the bus or other subscribers;
// it is isolated and logged.
type MemoryBus struct {
	mu     sync.RWMutex
	subs   map[*subscription]struct{}
	closed bool

	ob     *observe.Observability
	clk    clock.Clock
	buffer int
}

// Option configures a MemoryBus.
type Option func(*MemoryBus)

// WithObservability sets the logger used to report handler errors and panics
// (default: observe.Default()).
func WithObservability(o *observe.Observability) Option {
	return func(b *MemoryBus) {
		if o != nil {
			b.ob = o
		}
	}
}

// WithClock sets the time source used to stamp Message.Time when a publisher
// leaves it zero (default: clock.System).
func WithClock(c clock.Clock) Option {
	return func(b *MemoryBus) {
		if c != nil {
			b.clk = c
		}
	}
}

// WithBuffer sets the per-subscription mailbox depth (default defaultBuffer). A
// larger buffer tolerates burstier publishers at the cost of more retained
// memory; values <= 0 fall back to the default.
func WithBuffer(n int) Option {
	return func(b *MemoryBus) {
		if n > 0 {
			b.buffer = n
		}
	}
}

// NewMemory constructs an in-process Bus ready to use with zero configuration.
func NewMemory(opts ...Option) *MemoryBus {
	b := &MemoryBus{
		subs:   make(map[*subscription]struct{}),
		ob:     observe.Default(),
		clk:    clock.System{},
		buffer: defaultBuffer,
	}
	for _, o := range opts {
		o(b)
	}
	return b
}

var _ Bus = (*MemoryBus)(nil)

// Publish delivers m to every matching subscription's mailbox, in the caller's
// goroutine up to the hand-off and then asynchronously in each subscription's
// goroutine. It blocks only while a matching mailbox is full, and unblocks early
// if ctx is cancelled or the subscription is removed.
func (b *MemoryBus) Publish(ctx context.Context, m Message) error {
	if !ValidSubject(m.Subject) {
		return ErrInvalidSubject
	}
	if m.Time == 0 {
		m.Time = b.clk.Now().UnixNano()
	}

	b.mu.RLock()
	if b.closed {
		b.mu.RUnlock()
		return ErrClosed
	}
	// Snapshot the matching subscriptions so delivery does not hold the lock (a
	// full mailbox must not block Subscribe/Unsubscribe).
	targets := make([]*subscription, 0, len(b.subs))
	for s := range b.subs {
		if Match(s.subject, m.Subject) {
			targets = append(targets, s)
		}
	}
	b.mu.RUnlock()

	for _, s := range targets {
		select {
		case s.mailbox <- m:
		case <-s.done:
			// Unsubscribed mid-publish: skip it, do not error the publish.
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// Subscribe registers h under pattern and starts its delivery goroutine.
func (b *MemoryBus) Subscribe(_ context.Context, pattern string, h Handler) (Subscription, error) {
	if !ValidPattern(pattern) {
		return nil, ErrInvalidPattern
	}
	if h == nil {
		return nil, ErrInvalidPattern
	}

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil, ErrClosed
	}
	s := &subscription{
		bus:     b,
		subject: pattern,
		h:       h,
		mailbox: make(chan Message, b.buffer),
		done:    make(chan struct{}),
	}
	b.subs[s] = struct{}{}
	b.mu.Unlock()

	go s.run()
	return s, nil
}

// Close stops the bus: every subscription is unsubscribed and further Publish and
// Subscribe calls fail with ErrClosed.
func (b *MemoryBus) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	subs := make([]*subscription, 0, len(b.subs))
	for s := range b.subs {
		subs = append(subs, s)
	}
	b.subs = make(map[*subscription]struct{})
	b.mu.Unlock()

	for _, s := range subs {
		s.stop()
	}
	return nil
}

// subscription is one live registration with its own ordered mailbox.
type subscription struct {
	bus     *MemoryBus
	subject string
	h       Handler
	mailbox chan Message
	done    chan struct{}
	once    sync.Once
}

var _ Subscription = (*subscription)(nil)

// Subject implements Subscription.
func (s *subscription) Subject() string { return s.subject }

// Unsubscribe implements Subscription: it removes the subscription from the bus
// and stops its goroutine. Idempotent and concurrency-safe.
func (s *subscription) Unsubscribe() error {
	s.bus.mu.Lock()
	delete(s.bus.subs, s)
	s.bus.mu.Unlock()
	s.stop()
	return nil
}

// stop signals the delivery goroutine to exit exactly once. The mailbox is never
// closed, so a Publish racing with removal selects done instead of panicking on a
// closed channel; buffered messages are simply dropped (at-most-once delivery).
func (s *subscription) stop() {
	s.once.Do(func() { close(s.done) })
}

// run delivers mailbox messages to the handler in order until the subscription is
// stopped. done wins ties so a stop is prompt even with a backlog.
func (s *subscription) run() {
	for {
		select {
		case <-s.done:
			return
		default:
		}
		select {
		case <-s.done:
			return
		case m := <-s.mailbox:
			s.deliver(m)
		}
	}
}

// deliver runs the handler with a fresh context, isolating a handler panic or
// error so one bad subscriber cannot stall the mailbox or crash the bus.
func (s *subscription) deliver(m Message) {
	defer func() {
		if r := recover(); r != nil {
			s.bus.ob.Log.Error(context.Background(), "bus handler panicked",
				observe.String("subject", m.Subject), observe.String("pattern", s.subject))
		}
	}()
	if err := s.h(context.Background(), m); err != nil {
		s.bus.ob.Log.Warn(context.Background(), "bus handler returned error",
			observe.String("subject", m.Subject), observe.String("pattern", s.subject))
	}
}

package bus

import (
	"context"
	"strings"
	"sync"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/diag"
	"github.com/ionalpha/flynn/observe"
)

// defaultBuffer is the per-subscription mailbox depth. A full mailbox blocks
// Publish, so memory stays bounded and no message is dropped for a handler
// that keeps up.
const defaultBuffer = 64

// MemoryBus is the in-process [Bus] over Go channels, and the reference the
// NATS adapter is held to. Each subscription owns a goroutine draining its
// mailbox in publish order; a handler that panics or errors is logged and
// affects no other subscriber.
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

// WithObservability sets where handler errors and panics are logged (default
// observe.Default()).
func WithObservability(o *observe.Observability) Option {
	return func(b *MemoryBus) {
		if o != nil {
			b.ob = o
		}
	}
}

// WithClock sets the clock that stamps a zero Message.Time (default
// clock.System).
func WithClock(c clock.Clock) Option {
	return func(b *MemoryBus) {
		if c != nil {
			b.clk = c
		}
	}
}

// WithBuffer sets the per-subscription mailbox depth (default 64); n <= 0
// keeps the default.
func WithBuffer(n int) Option {
	return func(b *MemoryBus) {
		if n > 0 {
			b.buffer = n
		}
	}
}

// NewMemory returns a MemoryBus that needs no configuration.
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

// Publish hands m to every matching mailbox and returns; handlers run later on
// their own goroutines. It blocks only on a full mailbox, until ctx is
// cancelled or that subscription is removed.
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
	st := strings.Split(m.Subject, ".")
	// Snapshot the targets so a full mailbox does not block Subscribe or
	// Unsubscribe behind the lock.
	targets := make([]*subscription, 0, len(b.subs))
	for s := range b.subs {
		if matchTokens(s.tokens, st) {
			targets = append(targets, s)
		}
	}
	b.mu.RUnlock()

	for _, s := range targets {
		select {
		case s.mailbox <- m:
		case <-s.done:
			// Unsubscribed mid-publish: skip it without failing the publish.
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// Subscribe registers h under pattern and starts its delivery goroutine. A nil
// h is rejected as [ErrInvalidPattern].
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
		tokens:  strings.Split(pattern, "."),
		h:       h,
		mailbox: make(chan Message, b.buffer),
		done:    make(chan struct{}),
	}
	b.subs[s] = struct{}{}
	b.mu.Unlock()

	// The pump stops at Unsubscribe or Close, never at ctx cancellation.
	go s.run() //nolint:gosec // G118: a subscription pump is a background service, not request-scoped
	return s, nil
}

// Close unsubscribes everything; later Publish and Subscribe calls fail with
// [ErrClosed]. It is idempotent.
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
	tokens  []string // subject split at Subscribe; read-only after
	h       Handler
	mailbox chan Message
	done    chan struct{}
	once    sync.Once
}

var _ Subscription = (*subscription)(nil)

// Subject implements Subscription.
func (s *subscription) Subject() string { return s.subject }

// Unsubscribe implements Subscription.
func (s *subscription) Unsubscribe() error {
	s.bus.mu.Lock()
	delete(s.bus.subs, s)
	s.bus.mu.Unlock()
	s.stop()
	return nil
}

// stop closes done once. The mailbox is never closed, so a Publish racing
// removal selects done instead of panicking on a closed channel; buffered
// messages are dropped.
func (s *subscription) stop() {
	s.once.Do(func() { close(s.done) })
}

// run delivers mailbox messages to the handler in order until the subscription is
// stopped. done wins ties so a stop is prompt even with a backlog.
func (s *subscription) run() {
	// The label names the subject in a goroutine profile holding thousands of
	// pumps.
	diag.LabelGoroutine(context.Background(), "component", "bus", "subject", s.subject)

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

// deliver recovers a handler panic so one bad subscriber cannot stall its
// mailbox or crash the bus.
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

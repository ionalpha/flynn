// Package bustest is the conformance suite for bus.Bus. Every backend (the
// in-process MemoryBus default, a NATS adapter, a host's broker) runs RunSuite
// and must behave identically, so an opt-in broker is held to the exact pub/sub,
// subject-matching, ordering, and lifecycle contract of the reference MemoryBus
// rather than re-tested by hand.
package bustest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ionalpha/flynn/bus"
)

// deliverWait bounds how long a test waits for an asynchronous delivery before
// failing. Generous so a slow CI box does not flake, short enough to fail fast.
const deliverWait = 2 * time.Second

// RunSuite runs the full bus.Bus contract against buses built by newBus. Each
// subtest gets a fresh bus, closed at the end of the subtest.
func RunSuite(t *testing.T, newBus func() bus.Bus) {
	t.Helper()
	t.Run("DeliversToMatchingSubscriber", func(t *testing.T) { testDelivers(t, newBus()) })
	t.Run("StarMatchesOneToken", func(t *testing.T) { testStar(t, newBus()) })
	t.Run("TailMatchesTrailingTokens", func(t *testing.T) { testTail(t, newBus()) })
	t.Run("NonMatchingSubjectNotDelivered", func(t *testing.T) { testNoMatch(t, newBus()) })
	t.Run("FanOutToAllSubscribers", func(t *testing.T) { testFanOut(t, newBus()) })
	t.Run("OrderPreservedPerSubscription", func(t *testing.T) { testOrder(t, newBus()) })
	t.Run("UnsubscribeStopsDelivery", func(t *testing.T) { testUnsubscribe(t, newBus()) })
	t.Run("InvalidSubjectRejected", func(t *testing.T) { testInvalidSubject(t, newBus()) })
	t.Run("InvalidPatternRejected", func(t *testing.T) { testInvalidPattern(t, newBus()) })
	t.Run("ClosedBusRejects", func(t *testing.T) { testClosed(t, newBus()) })
	t.Run("HandlerErrorIsolated", func(t *testing.T) { testHandlerErrorIsolated(t, newBus()) })
}

// collector is a handler that records every message it receives, in order.
type collector struct {
	mu   sync.Mutex
	got  []bus.Message
	recv chan struct{}
}

func newCollector() *collector { return &collector{recv: make(chan struct{}, 1024)} }

func (c *collector) handle(_ context.Context, m bus.Message) error {
	c.mu.Lock()
	c.got = append(c.got, m)
	c.mu.Unlock()
	c.recv <- struct{}{}
	return nil
}

// waitFor blocks until the collector has received n messages or deliverWait
// elapses, then returns the messages received so far.
func (c *collector) waitFor(t *testing.T, n int) []bus.Message {
	t.Helper()
	deadline := time.After(deliverWait)
	for range n {
		select {
		case <-c.recv:
		case <-deadline:
			t.Fatalf("timed out waiting for %d messages; got %d", n, len(c.snapshot()))
		}
	}
	return c.snapshot()
}

// expectNone asserts no message arrives within a short window.
func (c *collector) expectNone(t *testing.T) {
	t.Helper()
	select {
	case <-c.recv:
		t.Fatalf("expected no delivery, got %d", len(c.snapshot()))
	case <-time.After(100 * time.Millisecond):
	}
}

func (c *collector) snapshot() []bus.Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]bus.Message(nil), c.got...)
}

func mustSubscribe(t *testing.T, b bus.Bus, pattern string, h bus.Handler) bus.Subscription {
	t.Helper()
	s, err := b.Subscribe(context.Background(), pattern, h)
	if err != nil {
		t.Fatalf("Subscribe(%q): %v", pattern, err)
	}
	return s
}

func mustPublish(t *testing.T, b bus.Bus, subject string, payload string) {
	t.Helper()
	if err := b.Publish(context.Background(), bus.Message{Subject: subject, Payload: []byte(payload)}); err != nil {
		t.Fatalf("Publish(%q): %v", subject, err)
	}
}

func testDelivers(t *testing.T, b bus.Bus) {
	defer func() { _ = b.Close() }()
	c := newCollector()
	mustSubscribe(t, b, "orders.created", c.handle)
	mustPublish(t, b, "orders.created", "hello")

	got := c.waitFor(t, 1)
	if len(got) != 1 || string(got[0].Payload) != "hello" {
		t.Fatalf("got %+v, want one message payload %q", got, "hello")
	}
}

func testStar(t *testing.T, b bus.Bus) {
	defer func() { _ = b.Close() }()
	c := newCollector()
	mustSubscribe(t, b, "orders.*", c.handle)
	mustPublish(t, b, "orders.created", "a")
	mustPublish(t, b, "orders.shipped", "b")
	// One token only: a deeper subject must not match "orders.*".
	mustPublish(t, b, "orders.created.line", "c")

	got := c.waitFor(t, 2)
	if len(got) != 2 {
		t.Fatalf("got %d messages, want 2 (deeper subject must not match *)", len(got))
	}
}

func testTail(t *testing.T, b bus.Bus) {
	defer func() { _ = b.Close() }()
	c := newCollector()
	mustSubscribe(t, b, "orders.>", c.handle)
	mustPublish(t, b, "orders.created", "a")
	mustPublish(t, b, "orders.created.line.1", "b")
	got := c.waitFor(t, 2)
	if len(got) != 2 {
		t.Fatalf("got %d messages, want 2 ('>' matches one or more trailing tokens)", len(got))
	}
}

func testNoMatch(t *testing.T, b bus.Bus) {
	defer func() { _ = b.Close() }()
	c := newCollector()
	mustSubscribe(t, b, "orders.created", c.handle)
	mustPublish(t, b, "orders.shipped", "x")
	c.expectNone(t)
}

func testFanOut(t *testing.T, b bus.Bus) {
	defer func() { _ = b.Close() }()
	c1, c2 := newCollector(), newCollector()
	mustSubscribe(t, b, "signals.>", c1.handle)
	mustSubscribe(t, b, "signals.tick", c2.handle)
	mustPublish(t, b, "signals.tick", "t")

	if got := c1.waitFor(t, 1); len(got) != 1 {
		t.Fatalf("c1 got %d, want 1", len(got))
	}
	if got := c2.waitFor(t, 1); len(got) != 1 {
		t.Fatalf("c2 got %d, want 1", len(got))
	}
}

func testOrder(t *testing.T, b bus.Bus) {
	defer func() { _ = b.Close() }()
	c := newCollector()
	mustSubscribe(t, b, "seq", c.handle)
	const n = 50
	for i := range n {
		mustPublish(t, b, "seq", itoa(i))
	}
	got := c.waitFor(t, n)
	// Payloads were published as ascending indices; delivery to one subscription
	// must preserve that order exactly.
	for i := range got {
		if pubIndex(got[i].Payload) != i {
			t.Fatalf("out of order: message %d has index %d (payload %q)", i, pubIndex(got[i].Payload), got[i].Payload)
		}
	}
}

func testUnsubscribe(t *testing.T, b bus.Bus) {
	defer func() { _ = b.Close() }()
	c := newCollector()
	s := mustSubscribe(t, b, "topic", c.handle)
	mustPublish(t, b, "topic", "before")
	c.waitFor(t, 1)

	if err := s.Unsubscribe(); err != nil {
		t.Fatalf("Unsubscribe: %v", err)
	}
	// Idempotent.
	if err := s.Unsubscribe(); err != nil {
		t.Fatalf("second Unsubscribe: %v", err)
	}
	mustPublish(t, b, "topic", "after")
	c.expectNone(t)
}

func testInvalidSubject(t *testing.T, b bus.Bus) {
	defer func() { _ = b.Close() }()
	for _, subj := range []string{"", "orders.", ".orders", "orders..created", "orders.*", "orders.>", "has space"} {
		if err := b.Publish(context.Background(), bus.Message{Subject: subj}); err == nil {
			t.Fatalf("Publish(%q) = nil error, want rejection", subj)
		}
	}
}

func testInvalidPattern(t *testing.T, b bus.Bus) {
	defer func() { _ = b.Close() }()
	noop := func(context.Context, bus.Message) error { return nil }
	for _, pat := range []string{"", "orders.", "a.>.b", "orders..created", "has space"} {
		if _, err := b.Subscribe(context.Background(), pat, noop); err == nil {
			t.Fatalf("Subscribe(%q) = nil error, want rejection", pat)
		}
	}
}

func testClosed(t *testing.T, b bus.Bus) {
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := b.Publish(context.Background(), bus.Message{Subject: "x"}); err == nil {
		t.Fatal("Publish after Close = nil error, want ErrClosed")
	}
	if _, err := b.Subscribe(context.Background(), "x", func(context.Context, bus.Message) error { return nil }); err == nil {
		t.Fatal("Subscribe after Close = nil error, want ErrClosed")
	}
	// Close is idempotent.
	if err := b.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func testHandlerErrorIsolated(t *testing.T, b bus.Bus) {
	defer func() { _ = b.Close() }()
	good := newCollector()
	mustSubscribe(t, b, "x", func(context.Context, bus.Message) error { return errors.New("boom") })
	mustSubscribe(t, b, "x", good.handle)
	mustPublish(t, b, "x", "ok")
	// The erroring subscriber must not prevent the healthy one from receiving.
	if got := good.waitFor(t, 1); len(got) != 1 {
		t.Fatalf("healthy subscriber got %d, want 1", len(got))
	}
}

// itoa and pubIndex avoid an strconv import in this helper and encode/decode the
// publish index embedded in the order test's payloads.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

func pubIndex(payload []byte) int {
	n := 0
	for _, c := range payload {
		if c >= '0' && c <= '9' {
			n = n*10 + int(c-'0')
		}
	}
	return n
}

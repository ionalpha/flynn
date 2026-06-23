package dispatch

import (
	"context"
	"sync"
)

// MemorySink is an EventSink that records events in memory. It stands in for the
// durable event spine until that lands, and makes the waist's emissions
// inspectable in tests. It is safe for concurrent use.
type MemorySink struct {
	mu     sync.Mutex
	events []Event
}

// Append implements EventSink.
func (s *MemorySink) Append(_ context.Context, e Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
	return nil
}

// Events returns a copy of the recorded events in append order.
func (s *MemorySink) Events() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Event, len(s.events))
	copy(out, s.events)
	return out
}

var _ EventSink = (*MemorySink)(nil)

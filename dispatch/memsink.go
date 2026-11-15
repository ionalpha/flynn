package dispatch

import (
	"context"
	"sync"
)

// MemorySink records events in memory where no spine sink is wired, so tests can
// inspect them. It is safe for concurrent use.
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

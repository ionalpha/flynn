package inbox

import "context"

// Source produces inbound signals from one origin: a chat platform, an email
// mailbox, a webhook listener, a monitor. Receive streams signals until ctx is
// cancelled, at which point the source closes the returned channel. Name
// identifies the source for routing replies back through the matching Sink. A
// Source need not set Spec.Source; the ingester stamps it from Name, so routing
// never depends on the adapter filling it in.
type Source interface {
	Name() string
	Receive(ctx context.Context) (<-chan Spec, error)
}

// Sink delivers an outbound message to a conversation on a source's platform, so a
// disposition can reply or notify on the channel a signal arrived on. Name matches
// the Source it pairs with, which is how the triage controller finds the right
// Sink for a signal's source.
type Sink interface {
	Name() string
	Send(ctx context.Context, conversation, text string) error
}

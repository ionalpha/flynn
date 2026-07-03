package inbox

import (
	"context"
	"time"

	"github.com/ionalpha/flynn/clock"
)

// Source produces inbound entries from one origin: a chat platform, an email
// mailbox, a webhook listener, a monitor. Receive streams entries until ctx is
// cancelled, at which point the source closes the returned channel. Name
// identifies the source for routing replies back through the matching Sink. A
// Source need not set Spec.Source; the ingester stamps it from Name, so routing
// never depends on the adapter filling it in.
type Source interface {
	Name() string
	Receive(ctx context.Context) (<-chan Spec, error)
}

// ReceiveLoop is the reconnect skeleton every long-lived Source shares: it runs
// attempt repeatedly until ctx is cancelled, pausing for backoff after a failed
// attempt so a dropped connection is retried without a busy loop. A nil error means
// the attempt ended cleanly (a long poll returned, a stream closed on cancellation)
// and the next attempt runs immediately; a non-nil error triggers the backoff wait.
// The wait is clock-driven, so a Manual clock makes reconnect timing deterministic
// under test instead of sleeping on the wall clock. Every source (Telegram, Signal,
// and future Discord/Slack/email adapters) drives its Receive goroutine through this
// rather than hand-rolling the same loop.
func ReceiveLoop(ctx context.Context, backoff time.Duration, clk clock.Timing, attempt func(context.Context) error) {
	for {
		if ctx.Err() != nil {
			return
		}
		if err := attempt(ctx); err != nil && ctx.Err() == nil {
			select {
			case <-ctx.Done():
				return
			case <-clk.After(backoff):
			}
		}
	}
}

// Sink delivers an outbound message to a conversation on a source's platform, so a
// disposition can reply or notify on the channel an entry arrived on. Name matches
// the Source it pairs with, which is how the triage controller finds the right
// Sink for an entry's source.
type Sink interface {
	Name() string
	Send(ctx context.Context, conversation, text string) error
}

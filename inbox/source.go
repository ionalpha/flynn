package inbox

import (
	"context"
	"time"

	"github.com/ionalpha/flynn/clock"
)

// Source produces entries from one origin, such as a chat platform or a webhook.
// Receive closes its channel when ctx is cancelled. Name pairs the source with its
// [Sink]; the ingester stamps it onto each entry.
type Source interface {
	Name() string
	Receive(ctx context.Context) (<-chan Spec, error)
}

// ReceiveLoop runs attempt until ctx is cancelled: at once after a nil return,
// after backoff on clk after an error, so a dropped connection is retried without
// a busy loop. Every long-lived [Source] uses it.
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

// Sink sends a message to a conversation on a platform. Its Name matches the
// [Source] it pairs with.
type Sink interface {
	Name() string
	Send(ctx context.Context, conversation, text string) error
}

package channel

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// defaultConcurrency bounds in-flight message handling when a caller does not set
// one. It keeps a burst of messages from launching an unbounded number of agent
// runs at once while still overlapping the slow, I/O-bound work.
const defaultConcurrency = 4

// runFailureReply is sent to the user when the Runner returns an error, so a
// failed request is acknowledged on the conversation rather than silently dropped.
// It is deliberately generic: details go to the error handler, not the chat.
const runFailureReply = "Sorry, I could not complete that request."

// Gateway drives a set of channels against one Runner. It fans in every channel's
// inbound messages, runs each as a goal through the Runner, and sends the reply
// back on the channel it arrived from. Messages are handled concurrently up to a
// bound, so one slow run does not stall the others. Run blocks until the context
// is cancelled, then drains the in-flight handlers before returning.
type Gateway struct {
	runner      Runner
	channels    []Channel
	concurrency int
	onError     func(error)
}

// Option configures a Gateway.
type Option func(*Gateway)

// WithConcurrency caps how many messages are handled at once. A non-positive value
// is ignored, leaving the default.
func WithConcurrency(n int) Option {
	return func(g *Gateway) {
		if n > 0 {
			g.concurrency = n
		}
	}
}

// WithErrorHandler registers a callback for non-fatal errors: a Runner failure, a
// Send failure, or a channel that will not start. The default discards them. The
// handler can be invoked from several goroutines at once and must be safe for that.
func WithErrorHandler(fn func(error)) Option {
	return func(g *Gateway) {
		if fn != nil {
			g.onError = fn
		}
	}
}

// NewGateway builds a Gateway that routes messages from channels through runner.
func NewGateway(runner Runner, channels []Channel, opts ...Option) *Gateway {
	g := &Gateway{
		runner:      runner,
		channels:    channels,
		concurrency: defaultConcurrency,
		onError:     func(error) {},
	}
	for _, o := range opts {
		o(g)
	}
	return g
}

// Run starts every channel and routes messages until ctx is cancelled. Each
// channel that fails to start is reported and skipped; if none start, Run returns
// an error rather than blocking forever. Once ctx is cancelled the adapters close
// their inbound streams, Run stops accepting new messages, waits for the handlers
// already running, and returns ctx.Err().
func (g *Gateway) Run(ctx context.Context) error {
	if len(g.channels) == 0 {
		return errors.New("channel: gateway has no channels")
	}

	sem := make(chan struct{}, g.concurrency)
	var handlers sync.WaitGroup // in-flight message handlers
	var readers sync.WaitGroup  // per-channel fan-in readers
	started := 0

	for _, ch := range g.channels {
		in, err := ch.Receive(ctx)
		if err != nil {
			g.onError(fmt.Errorf("channel %s: receive: %w", ch.Name(), err))
			continue
		}
		started++
		readers.Add(1)
		go func(ch Channel, in <-chan Inbound) {
			defer readers.Done()
			for msg := range in {
				// Stamp the source so the convo id and reply routing do not depend on
				// the adapter setting it.
				msg.Channel = ch.Name()
				select {
				case sem <- struct{}{}:
				case <-ctx.Done():
					return
				}
				handlers.Add(1)
				go func(msg Inbound) {
					defer handlers.Done()
					defer func() { <-sem }()
					g.handle(ctx, ch, msg)
				}(msg)
			}
		}(ch, in)
	}

	if started == 0 {
		return errors.New("channel: no channels could start")
	}

	readers.Wait()  // every inbound stream closed (ctx cancelled or adapter stopped)
	handlers.Wait() // in-flight handlers drained
	return ctx.Err()
}

// handle runs one message as a goal and sends the reply back on its channel. A
// Runner error is reported and acknowledged to the user with a generic reply; a
// Send error is reported. An empty successful reply is dropped, so a Runner can
// choose to stay silent.
func (g *Gateway) handle(ctx context.Context, ch Channel, msg Inbound) {
	convo := msg.Channel + ":" + msg.Chat
	reply, err := g.runner.Run(ctx, convo, msg.Text)
	if err != nil {
		g.onError(fmt.Errorf("channel %s: run: %w", ch.Name(), err))
		reply = runFailureReply
	}
	if reply == "" {
		return
	}
	if err := ch.Send(ctx, Outbound{Chat: msg.Chat, Text: reply}); err != nil {
		g.onError(fmt.Errorf("channel %s: send: %w", ch.Name(), err))
	}
}

// Package channel lets the agent live where its users already are. A chat
// platform delivers a message, the agent runs it as a goal, and the reply is sent
// back on the same conversation. Each platform is adapted to one small port
// (Channel); a Gateway drives any set of channels against one Runner, so adding a
// platform is a thin adapter rather than new agent code.
//
// The package depends only on the Runner interface, never on the agent runtime,
// so the engine wiring and the messaging surface stay decoupled: a host builds a
// Runner from its own assembly and hands it, plus the channels, to the Gateway.
package channel

import "context"

// Inbound is a message received from a channel: the user's text plus the address
// needed to reply. Chat identifies the conversation on the source platform and is
// the reply address; Channel names the adapter it arrived on, so a Gateway driving
// several platforms routes the reply back to the right one. User is the platform
// user id or handle, kept for routing and audit and may be empty.
type Inbound struct {
	Channel string
	Chat    string
	User    string
	Text    string
}

// Outbound is a reply to deliver to a conversation on a channel.
type Outbound struct {
	Chat string
	Text string
}

// Channel is one chat platform adapted to the gateway. Receive streams inbound
// messages until ctx is cancelled, at which point the adapter closes the returned
// channel; Send delivers a reply. Name identifies the adapter for routing and
// logging and must be stable and unique within a Gateway. An adapter owns its own
// transport, whether long-polling or a webhook, behind Receive.
type Channel interface {
	Name() string
	Receive(ctx context.Context) (<-chan Inbound, error)
	Send(ctx context.Context, out Outbound) error
}

// Runner turns a user's message into a reply by running it as an agent goal. convo
// is a stable per-conversation id (the channel name and chat id combined) so an
// implementation may keep continuity across messages; it returns the agent's final
// answer. The Gateway depends only on this, so the agent internals stay behind it.
type Runner interface {
	Run(ctx context.Context, convo, text string) (reply string, err error)
}

// RunnerFunc adapts an ordinary function to a Runner.
type RunnerFunc func(ctx context.Context, convo, text string) (string, error)

// Run calls f.
func (f RunnerFunc) Run(ctx context.Context, convo, text string) (string, error) {
	return f(ctx, convo, text)
}

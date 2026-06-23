// Package bus is the agent's pub/sub messaging port: ambient, fire-and-forget
// signals that decouple producers from consumers. Proactive mode ("flynn watch"),
// cross-component notifications, and fleet/k8s event fan-out all publish to and
// subscribe on subjects rather than calling each other directly.
//
// Like state, spine, and observe, messaging is a PORT with a zero-dependency
// in-process default (MemoryBus, Go channels) and heavy backends as opt-in
// adapters (NATS/JetStream for multi-process, fleet, and k8s fan-out). The
// subject grammar mirrors NATS so that adapter is a drop-in: dot-separated
// non-empty tokens, with `*` matching exactly one token and `>` (only as the
// final token) matching one or more trailing tokens.
//
// The bus is deliberately distinct from its neighbours. The spine is the durable,
// ordered truth log; the jobs queue is durable, retried work. The bus is none of
// those: delivery is at-most-once, unordered across subscribers (but ordered
// within one), and not itself durable. When a signal must survive a restart it is
// recorded on the spine; the bus only moves signals between live components.
package bus

import (
	"context"
	"strings"

	"github.com/ionalpha/flynn/fault"
)

// Message is one published signal. Payload is opaque bytes so the wire format is
// the caller's choice and the NATS adapter passes it through unchanged.
type Message struct {
	// Subject is the concrete, wildcard-free address the message is published to.
	Subject string
	// Payload is the opaque message body.
	Payload []byte
	// Time is when the message was published; the bus stamps it from its clock
	// when zero.
	Time int64 // unix nanos

	// Trace linkage carries the originating span/causation across the bus, so a
	// signal published in one component stays correlated with the work it
	// triggers. These mirror spine.Event and may be empty.
	TraceID     string
	SpanID      string
	CausationID string

	// OriginInstanceID is the instance that published the message, so fleet
	// fan-out can attribute and de-loop it. May be empty for single-instance use.
	OriginInstanceID string
}

// Handler processes a delivered message. It runs in the bus's delivery
// goroutine, not the publisher's, so it must not assume the publisher is still
// waiting. A returned error is observable (logged at the bus) but not retried:
// the bus is at-most-once, so durable retry belongs to the jobs queue, not here.
type Handler func(ctx context.Context, m Message) error

// Subscription is one active subscription. Unsubscribe stops delivery and is
// idempotent; after it returns no further handler calls begin.
type Subscription interface {
	// Subject returns the pattern the subscription was created with.
	Subject() string
	// Unsubscribe stops delivery to this subscription. It is safe to call more
	// than once and from any goroutine.
	Unsubscribe() error
}

// Bus is the pub/sub port. Publish delivers a message to every subscription whose
// pattern matches its subject; a message with no matching subscription is
// dropped. Implementations must be safe for concurrent use.
type Bus interface {
	// Publish delivers m to all matching subscriptions. The subject must be a
	// concrete subject (no wildcards); an invalid subject returns an error and
	// delivers nothing.
	Publish(ctx context.Context, m Message) error
	// Subscribe registers h for every message whose subject matches pattern. The
	// pattern may use the `*` and `>` wildcards; an invalid pattern returns an
	// error.
	Subscribe(ctx context.Context, pattern string, h Handler) (Subscription, error)
	// Close stops the bus and all its subscriptions. Publish and Subscribe fail
	// after Close.
	Close() error
}

// Sentinel errors. They are fault-classified so callers branch on the class
// (see fault.Classify) rather than on string matching.
var (
	// ErrClosed is returned by Publish/Subscribe after the bus is closed.
	ErrClosed = fault.New(fault.Terminal, "bus_closed", "bus is closed")
	// ErrInvalidSubject is returned when a published subject is empty or contains
	// wildcards or malformed tokens.
	ErrInvalidSubject = fault.New(fault.Terminal, "bus_invalid_subject", "invalid subject")
	// ErrInvalidPattern is returned when a subscription pattern is malformed.
	ErrInvalidPattern = fault.New(fault.Terminal, "bus_invalid_pattern", "invalid subscription pattern")
)

// Wildcard tokens, mirroring NATS.
const (
	// TokenAny ("*") matches exactly one token at its position.
	TokenAny = "*"
	// TokenTail (">") matches one or more trailing tokens; valid only as the
	// final pattern token.
	TokenTail = ">"
)

// Match reports whether subject matches pattern under the NATS-style grammar:
// tokens are separated by ".", "*" matches exactly one token, and ">" (only as
// the final token) matches one or more trailing tokens. It is total: any pair of
// strings yields a bool and never panics, so it is safe on untrusted input.
func Match(pattern, subject string) bool {
	if pattern == "" || subject == "" {
		return false
	}
	pt := strings.Split(pattern, ".")
	st := strings.Split(subject, ".")
	for i, p := range pt {
		switch p {
		case TokenTail:
			// ">" is only meaningful as the final pattern token, where it matches
			// one or more remaining subject tokens. Anywhere else it cannot match.
			return i == len(pt)-1 && i < len(st)
		case TokenAny:
			if i >= len(st) {
				return false
			}
		default:
			if i >= len(st) || p != st[i] {
				return false
			}
		}
	}
	return len(pt) == len(st)
}

// ValidSubject reports whether s is a legal concrete subject to publish to:
// one or more non-empty, wildcard-free, whitespace-free tokens.
func ValidSubject(s string) bool {
	if s == "" {
		return false
	}
	for _, tok := range strings.Split(s, ".") {
		if !validToken(tok) || tok == TokenAny || tok == TokenTail {
			return false
		}
	}
	return true
}

// ValidPattern reports whether s is a legal subscription pattern: one or more
// non-empty, whitespace-free tokens, where "*" may appear at any position and
// ">" only as the final token.
func ValidPattern(s string) bool {
	if s == "" {
		return false
	}
	toks := strings.Split(s, ".")
	for i, tok := range toks {
		switch tok {
		case TokenAny:
			// fine at any position
		case TokenTail:
			if i != len(toks)-1 {
				return false
			}
		default:
			if !validToken(tok) {
				return false
			}
		}
	}
	return true
}

// validToken reports whether tok is a non-empty token free of whitespace and the
// token separator. The wildcard tokens are handled by the callers, which decide
// whether a wildcard is legal in that position.
func validToken(tok string) bool {
	if tok == "" {
		return false
	}
	return !strings.ContainsAny(tok, " \t\r\n.")
}

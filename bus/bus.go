// Package bus is fire-and-forget pub/sub between live components: "flynn
// watch", cross-component notices and fleet fan-out publish on subjects
// instead of calling each other.
//
// [MemoryBus] is the in-process default; a NATS adapter is the multi-process
// backend, so the subject grammar is NATS's: dot-separated non-empty tokens,
// `*` matching one token and a final `>` matching one or more.
//
// Delivery is at-most-once, ordered within a subscription and unordered across
// them, and nothing is stored. A signal that must survive a restart goes on the
// spine; durable retried work goes on the jobs queue.
package bus

import (
	"context"
	"strings"

	"github.com/ionalpha/flynn/fault"
)

// Message is one published signal. Payload is opaque bytes so the wire format is
// the caller's choice and the NATS adapter passes it through unchanged.
type Message struct {
	// Subject is concrete: no wildcards.
	Subject string
	Payload []byte
	// Time is unix nanoseconds; the bus stamps it from its clock when zero.
	Time int64

	// The trace fields mirror spine.Event so a signal stays correlated with
	// the work it triggers. Any may be empty.
	TraceID     string
	SpanID      string
	CausationID string

	// OriginInstanceID is the instance that published the message, so fleet
	// fan-out can attribute and de-loop it. May be empty for single-instance use.
	OriginInstanceID string
}

// Handler processes a delivered message on the bus's delivery goroutine, so the
// publisher may have moved on. A returned error is logged and never retried.
type Handler func(ctx context.Context, m Message) error

// Subscription is one active subscription. Unsubscribe stops delivery and is
// idempotent; after it returns no further handler calls begin.
type Subscription interface {
	// Subject returns the pattern the subscription was created with.
	Subject() string
	// Unsubscribe is safe to call more than once and from any goroutine.
	Unsubscribe() error
}

// Bus is the pub/sub port. A message with no matching subscription is dropped.
// Implementations must be safe for concurrent use; bustest.RunSuite is the
// contract.
type Bus interface {
	// Publish delivers m to every subscription whose pattern matches its
	// subject. An invalid or wildcard subject is rejected and delivers nothing.
	Publish(ctx context.Context, m Message) error
	// Subscribe registers h under a pattern that may use `*` and `>`.
	Subscribe(ctx context.Context, pattern string, h Handler) (Subscription, error)
	// Close stops every subscription; Publish and Subscribe fail after it.
	Close() error
}

// Sentinel errors, fault-classified so callers branch with fault.Classify.
var (
	ErrClosed         = fault.New(fault.Terminal, "bus_closed", "bus is closed")
	ErrInvalidSubject = fault.New(fault.Terminal, "bus_invalid_subject", "invalid subject")
	ErrInvalidPattern = fault.New(fault.Terminal, "bus_invalid_pattern", "invalid subscription pattern")
)

// Wildcard tokens, as in NATS.
const (
	// TokenAny matches exactly one token.
	TokenAny = "*"
	// TokenTail matches one or more trailing tokens and is valid only last.
	TokenTail = ">"
)

// Match reports whether subject matches pattern. It never panics, so it is safe
// on untrusted input.
func Match(pattern, subject string) bool {
	if pattern == "" || subject == "" {
		return false
	}
	return matchTokens(strings.Split(pattern, "."), strings.Split(subject, "."))
}

// matchTokens is Match over pre-split tokens, so a publish to N subscribers
// splits the subject once. Both slices must be non-empty.
func matchTokens(pt, st []string) bool {
	for i, p := range pt {
		switch p {
		case TokenTail:
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

// ValidSubject reports whether s is publishable: non-empty tokens with no
// wildcard or whitespace.
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

// ValidPattern reports whether s is a legal subscription pattern: non-empty,
// whitespace-free tokens with ">" only last.
func ValidPattern(s string) bool {
	if s == "" {
		return false
	}
	toks := strings.Split(s, ".")
	for i, tok := range toks {
		switch tok {
		case TokenAny:
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

// validToken accepts wildcards; the callers decide where one is legal.
func validToken(tok string) bool {
	if tok == "" {
		return false
	}
	return !strings.ContainsAny(tok, " \t\r\n.")
}

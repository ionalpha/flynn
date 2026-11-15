// Package secret holds credentials in a [Text] that renders as [Redacted] under
// every fmt verb, JSON and text encoding, so a stray %v or a logged struct
// cannot leak a key. The value leaves only through [Text.Expose]. [Source]
// resolves a credential by name.
//
// Redaction is exact; wiping is best-effort, because [Text.Destroy] cannot reach
// copies the runtime or an earlier Expose made.
package secret

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
)

// Redacted is what every rendering of a [Text] shows. It is non-empty so a
// redacted secret does not read as a missing field.
const Redacted = "[REDACTED]"

// Text holds a secret value. The zero value is an empty secret. Copies share
// their bytes, so Destroy on one clears all. Reads are safe concurrently;
// Destroy must not race Expose.
type Text struct {
	b []byte
}

// New copies value into storage the [Text] owns.
func New(value string) Text {
	if value == "" {
		return Text{}
	}
	return Text{b: []byte(value)}
}

// Expose returns the plaintext. Call it only where the value leaves the process
// (a request header), and never log, format or store the result. It is the one
// call an audit greps for.
func (t Text) Expose() string { return string(t.b) }

// Empty reports whether the secret holds no value.
func (t Text) Empty() bool { return len(t.b) == 0 }

// Equal compares in constant time so timing does not leak the value. Two empty
// secrets are equal.
func (t Text) Equal(other Text) bool {
	return subtle.ConstantTimeCompare(t.b, other.b) == 1
}

// Destroy zeroes the bytes every copy shares and empties the receiver. It cannot
// reach a copy the garbage collector made or a string Expose returned.
func (t *Text) Destroy() {
	for i := range t.b {
		t.b[i] = 0
	}
	t.b = nil
}

// Format writes [Redacted] for every verb. fmt consults it before Stringer and
// reflection, so %d and %x cannot print the byte slice.
func (t Text) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, Redacted) }

// String returns [Redacted], for callers that use Stringer outside fmt.
func (t Text) String() string { return Redacted }

// GoString returns [Redacted].
func (t Text) GoString() string { return Redacted }

// MarshalText returns [Redacted], so a Text field redacts under any
// TextMarshaler-aware encoding without help from its parent struct.
func (t Text) MarshalText() ([]byte, error) { return []byte(Redacted), nil }

// MarshalJSON encodes [Redacted]. With Format it covers the JSON and text slog
// handlers.
func (t Text) MarshalJSON() ([]byte, error) { return json.Marshal(Redacted) }

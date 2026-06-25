// Package secret is the agent's in-memory credential primitive: a string-like
// value that holds an API key, token, or password but refuses to render itself.
// Every way a value normally escapes a process by accident - fmt verbs, JSON
// encoding, structured logs, an event written to the spine - returns a fixed
// redaction marker instead of the secret. The value leaves only through one
// explicit, grep-able call, Expose, used at the single point where it crosses the
// network boundary (an Authorization header). A credential leak by stray %v or a
// logged struct is therefore not a bug to be careful about, it is unrepresentable.
//
// Text pairs with Source, the vault seam: where a named credential reference
// resolves to a value. The default EnvSource reads the process environment so the
// agent runs with zero setup; an OS keychain, an age-encrypted file vault, a KMS,
// or a remote broker are swap-in adapters of the same port, the credential analog
// of state.Provider.
//
// Honest scope: Go strings and the garbage collector make true zeroization
// best-effort. Text stores its bytes in a slice it owns and Destroy overwrites
// them, but copies the runtime made before Destroy (or that escaped via Expose)
// are beyond reach. The redaction guarantees are exact; the wiping is mitigation.
package secret

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
)

// Redacted is what every rendering of a Text shows in place of the value. It is
// deliberately not empty so a redacted secret is visible as such in output rather
// than looking like a missing field.
const Redacted = "[REDACTED]"

// Text holds a secret value and renders only as Redacted. The zero value is a
// valid empty secret. Text is a value type; copying it shares the underlying
// bytes, so Destroy on any copy clears them for all. It is safe to read
// concurrently; Destroy must not race a concurrent Expose.
type Text struct {
	b []byte
}

// New wraps a plaintext value in a Text. The input string's bytes are copied into
// storage the Text owns, so the caller may discard or overwrite its own copy.
func New(value string) Text {
	if value == "" {
		return Text{}
	}
	return Text{b: []byte(value)}
}

// Expose returns the underlying plaintext. This is the one audited point where a
// secret becomes an ordinary string: call sites are intentionally easy to grep,
// and it should be used only where the value must cross a boundary the agent does
// not control (a request header). Do not log, format, or store the result.
func (t Text) Expose() string { return string(t.b) }

// Empty reports whether the secret holds no value.
func (t Text) Empty() bool { return len(t.b) == 0 }

// Equal compares two secrets in constant time, so a comparison does not leak the
// value through timing. Two empty secrets are equal.
func (t Text) Equal(other Text) bool {
	return subtle.ConstantTimeCompare(t.b, other.b) == 1
}

// Destroy overwrites the secret's bytes with zeros and empties the receiver.
// Because copies of a Text share the same backing array, the wipe clears the
// value for every copy; the receiver is additionally reset so it reports Empty. It
// is best-effort (see the package doc): it cannot reach copies the runtime made
// during a GC move, or a string an earlier Expose returned.
func (t *Text) Destroy() {
	for i := range t.b {
		t.b[i] = 0
	}
	t.b = nil
}

// Format implements fmt.Formatter, which fmt consults before Stringer, GoStringer,
// or struct-field reflection for every verb. It writes Redacted unconditionally,
// so even a numeric verb (%d, %x) that would otherwise recurse into the byte slice
// and print its contents cannot reveal the value.
func (t Text) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, Redacted) }

// String implements fmt.Stringer and returns Redacted, for callers that invoke
// Stringer directly rather than through fmt (where Format already guards).
func (t Text) String() string { return Redacted }

// GoString implements fmt.GoStringer so the %#v verb (used by test helpers and
// struct dumps) also redacts.
func (t Text) GoString() string { return Redacted }

// MarshalText makes Text redact under any encoding that honors
// encoding.TextMarshaler, and lets a struct field of type Text serialize safely
// without a custom marshaler on the parent.
func (t Text) MarshalText() ([]byte, error) { return []byte(Redacted), nil }

// MarshalJSON ensures a Text encodes as the redaction marker string, never the
// value, so a secret embedded in a struct cannot leak into JSON written to a log,
// an API response, or the event spine. Together with Format (which guards every
// fmt verb) this covers both the JSON and the text rendering paths a structured
// logger uses, so a Text logged through any handler shows only the marker.
func (t Text) MarshalJSON() ([]byte, error) { return json.Marshal(Redacted) }

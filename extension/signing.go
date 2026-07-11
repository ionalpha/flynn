package extension

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"

	"github.com/ionalpha/flynn/fault"
)

// HostSigner produces detached signatures with a host-held key on behalf of a mounted
// extension tool. The tool builds something that needs a signature from a key it must not
// hold; it hands out the bytes and the host signs them. Public identifies the key so the
// tool can build against it; Sign returns a detached signature over the exact payload. The
// method behind this port (a local ed25519 key, a vault, a hardware token) is not the
// extension's concern, and the private key never crosses to the extension process.
type HostSigner interface {
	// Public is the public half of the signing key, as raw bytes.
	Public() ed25519.PublicKey
	// Sign returns a detached signature over payload.
	Sign(payload []byte) ([]byte, error)
}

// Ed25519HostSigner is the default HostSigner over an ed25519 private key the host supplies
// (from its vault or keychain). It only signs; it does not load, generate, or store the key.
type Ed25519HostSigner struct{ priv ed25519.PrivateKey }

// NewEd25519HostSigner builds a signer over an existing private key. It refuses a malformed
// key so a signer is never silently unable to sign.
func NewEd25519HostSigner(priv ed25519.PrivateKey) (*Ed25519HostSigner, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, fault.New(fault.Terminal, "extension_bad_signing_key", "extension: malformed ed25519 signing key")
	}
	return &Ed25519HostSigner{priv: priv}, nil
}

// Public returns the signing key's public half.
func (s *Ed25519HostSigner) Public() ed25519.PublicKey {
	return s.priv.Public().(ed25519.PublicKey)
}

// Sign returns the detached ed25519 signature over payload. ed25519 signing is deterministic,
// so no randomness is drawn.
func (s *Ed25519HostSigner) Sign(payload []byte) ([]byte, error) {
	return ed25519.Sign(s.priv, payload), nil
}

var _ HostSigner = (*Ed25519HostSigner)(nil)

// The host-signing handshake lets a mounted tool obtain host signatures without holding the
// key, as a generic capability with no chain- or format-specific knowledge in the host.
//
// A signing-enabled tool, instead of returning a final result, may return a signing message:
// a session token plus a base64 payload to sign. The host signs the payload with the tool's
// granted HostSigner and re-invokes the tool with the same session token and the detached
// signature, base64-encoded. This repeats until the tool returns a result with no signing
// message, which is the terminal result handed back to the caller unchanged. On the first
// call the host injects the granted key's public bytes under hostKeyField so the tool can
// build against the key it does not hold. Payload and signature are base64 of raw bytes, so
// the host reads and produces only opaque bytes.
const (
	// hostKeyField is injected into the first call: base64 of the granted key's public bytes.
	hostKeyField = "_hostKey"
	sessionField = "session"
	signatureKey = "signature"
	signErrorKey = "signError"
)

// signingReply is the subset of a tool result the host reads to drive the handshake. A reply
// whose Sign is nil (or that does not parse) is terminal; everything else in the result stays
// opaque to the host and passes through untouched.
type signingReply struct {
	Session string `json:"session"`
	Sign    *struct {
		Message string `json:"message"` // base64 of the bytes to sign
	} `json:"sign"`
}

// injectHostKey returns input with the granted key's public bytes added under hostKeyField,
// so a signing-enabled tool learns the key it will build against on its first call. An empty
// or null input starts from an empty object.
func injectHostKey(input json.RawMessage, pub ed25519.PublicKey) (json.RawMessage, error) {
	obj := map[string]json.RawMessage{}
	if len(input) > 0 && string(input) != "null" {
		if err := json.Unmarshal(input, &obj); err != nil {
			return nil, fault.Wrap(fault.Terminal, "extension_sign_input", err)
		}
	}
	enc, err := json.Marshal(base64.StdEncoding.EncodeToString(pub))
	if err != nil {
		return nil, fault.Wrap(fault.Terminal, "extension_sign_input", err)
	}
	obj[hostKeyField] = enc
	return json.Marshal(obj)
}

// resumeInput builds the follow-up call carrying the session token and either the detached
// signature (base64) or a signing-failure message, so the tool can continue or run its
// failure path.
func resumeInput(session string, sig []byte, signErr error) (json.RawMessage, error) {
	obj := map[string]any{sessionField: session}
	if signErr != nil {
		obj[signErrorKey] = signErr.Error()
	} else {
		obj[signatureKey] = base64.StdEncoding.EncodeToString(sig)
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return nil, fault.Wrap(fault.Terminal, "extension_sign_input", err)
	}
	return b, nil
}

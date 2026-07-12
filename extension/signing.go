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

// The host-call handshake lets a mounted tool borrow two host authorities it must not hold
// itself: a signing key, and the network. Both are generic capabilities; the host carries no
// chain-, protocol-, or format-specific knowledge, and reads only opaque bytes.
//
// A handshake-enabled tool, instead of returning a final result, may return a host-call
// message: a session token plus exactly one request.
//
//	sign  a base64 payload to sign. The host signs it with the tool's granted HostSigner and
//	      resumes the tool with the detached signature.
//	fetch a base64 request body to send. The host sends it with the tool's granted HostFetcher,
//	      which holds the destination, and resumes the tool with the response body.
//
// The host resumes the tool with the same session token and the result, and this repeats
// until the tool returns a result carrying no host-call message. That result is terminal and
// is handed back to the caller unchanged. On the first call the host injects the granted key's
// public bytes under hostKeyField so the tool can build against the key it does not hold.
//
// The tool names no destination. The endpoint lives in the host's HostFetcher grant, so a
// hostile extension cannot choose where its bytes go: the network authority it borrows is
// "reach the one place the operator granted", never "reach an address of my choosing". This is
// what lets an extension run with its own egress fully denied on every platform.
const (
	// hostKeyField is injected into the first call: base64 of the granted key's public bytes.
	hostKeyField = "_hostKey"
	sessionField = "session"
	signatureKey = "signature"
	signErrorKey = "signError"
	responseKey  = "response"
	fetchErrKey  = "fetchError"
)

// hostCallReply is the subset of a tool result the host reads to drive the handshake. A reply
// with neither Sign nor Fetch (or that does not parse) is terminal; everything else in the
// result stays opaque to the host and passes through untouched.
type hostCallReply struct {
	Session string `json:"session"`
	Sign    *struct {
		Message string `json:"message"` // base64 of the bytes to sign
	} `json:"sign"`
	Fetch *struct {
		Body string `json:"body"` // base64 of the request body to send
	} `json:"fetch"`
}

// parseHostCall reads a tool result as a host-call message. The result is UNTRUSTED: it is
// whatever an extension subprocess wrote to its stdout. A result that is not valid JSON, or is
// JSON carrying neither a sign nor a fetch block, is not a host-call message at all, which is
// how a terminal result is recognised. So a parse failure is not an error here, and the zero
// value it returns is exactly the "this is terminal" answer.
func parseHostCall(text string) hostCallReply {
	var reply hostCallReply
	_ = json.Unmarshal([]byte(text), &reply)
	return reply
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

// resumeSign builds the follow-up call carrying the session token and either the detached
// signature (base64) or a signing-failure message, so the tool can continue or run its
// failure path.
func resumeSign(session string, sig []byte, signErr error) (json.RawMessage, error) {
	if signErr != nil {
		return resume(session, signErrorKey, signErr.Error())
	}
	return resume(session, signatureKey, base64.StdEncoding.EncodeToString(sig))
}

// resumeFetch builds the follow-up call carrying the session token and either the response
// body (base64) or the error the host hit sending the request. A failure is delivered to the
// tool rather than aborting the call here, so the tool's own failure path runs: a mint that
// cannot reach the network must still unwind (revoking anything it created) rather than hang.
func resumeFetch(session string, body []byte, fetchErr error) (json.RawMessage, error) {
	if fetchErr != nil {
		return resume(session, fetchErrKey, fetchErr.Error())
	}
	return resume(session, responseKey, base64.StdEncoding.EncodeToString(body))
}

// resume builds a follow-up input: the session token plus exactly one result field.
func resume(session, key, value string) (json.RawMessage, error) {
	b, err := json.Marshal(map[string]any{sessionField: session, key: value})
	if err != nil {
		return nil, fault.Wrap(fault.Terminal, "extension_hostcall_input", err)
	}
	return b, nil
}

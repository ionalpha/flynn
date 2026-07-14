package extension

import (
	"context"
	"encoding/base64"
	"encoding/json"

	"github.com/ionalpha/flynn/fault"
)

// HostSigner produces detached signatures on behalf of a mounted extension tool, with a key
// the tool must not hold. The tool builds something that needs a signature; it hands out the
// bytes and gets a signature back. Public identifies the key so the tool can build against
// it; Sign returns a detached signature over the exact payload. What sits behind this port
// (a key held by another extension, a hardware token) is not the tool's concern, and the
// private key never crosses to the tool's process.
//
// Public returns raw bytes rather than a key of any particular type: signers sit on different
// curves, and they all have to travel the same port. The host never interprets these bytes, so
// it does not need to know which curve produced them, and gains nothing by knowing.
//
// Sign takes a context because the key need not be in this process: satisfying it may mean
// calling out to a signer extension, which can block and must be cancellable.
type HostSigner interface {
	// Public is the public half of the signing key, as raw bytes.
	Public() []byte
	// Sign returns a detached signature over payload.
	Sign(ctx context.Context, payload []byte) ([]byte, error)
}

// SelfPolicing is implemented by a HostSigner that decides for itself whether a payload may be
// signed, because it holds the key AND understands the payload's format. A signer extension is
// the case that matters: it parses the transaction and refuses an unsafe one at the point the
// key is used, which is the only point where the question can honestly be asked.
//
// The host REQUIRES this of any signer it uses (see serviceSign). It is not a convenience. The
// host holds no parser for any format, so a signer that does not judge its own payloads leaves
// nobody judging them, and signing bytes nobody has read is how the largest theft in this
// industry happened: a published, correctly-signed component was compromised upstream, and
// everyone who trusted its output signed what it asked them to.
//
// Verifying an extension's signature proves which binary is running. It proves nothing about
// what that binary asks to be signed. Those are different questions, and only the second one is
// asked at the moment the key is used, by whoever holds the key.
type SelfPolicing interface {
	// PolicesPayloads reports that this signer applies its own policy before signing.
	PolicesPayloads() bool
}

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
// or null input starts from an empty object. The bytes are opaque here: whichever curve the
// signer sits on, the host copies them through without interpreting them.
func injectHostKey(input json.RawMessage, pub []byte) (json.RawMessage, error) {
	obj := map[string]json.RawMessage{}
	if len(input) > 0 {
		if err := json.Unmarshal(input, &obj); err != nil {
			return nil, fault.Wrap(fault.Terminal, "extension_sign_input", err)
		}
		// Unmarshalling a JSON null into a map leaves the map nil, and JSON null is any
		// of "null", " null", "null\n", and so on. Comparing the raw bytes to "null"
		// catches one spelling of it, so a caller could hand the host a whitespace-padded
		// null and panic it on the assignment below. Ask the decoder what it produced
		// rather than trying to guess it from the input text.
		if obj == nil {
			obj = map[string]json.RawMessage{}
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

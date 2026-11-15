package extension

import (
	"context"
	"encoding/base64"
	"encoding/json"

	"github.com/ionalpha/flynn/fault"
)

// HostSigner signs for a mounted tool with a key the tool never holds. Public
// is raw bytes so signers on any curve share the port; the host never reads
// them. Sign takes a context because the key may sit in another process and
// the call must be cancellable.
type HostSigner interface {
	Public() []byte
	// Sign returns a detached signature over exactly payload.
	Sign(ctx context.Context, payload []byte) ([]byte, error)
}

// SelfPolicing is a [HostSigner] that understands its payload format and
// refuses unsafe payloads where the key is used. The host requires it of every
// signer, because the host parses no format and would otherwise sign bytes
// nobody read. A verified binary signature says which code runs, and nothing
// about what that code asks to have signed.
type SelfPolicing interface {
	// PolicesPayloads reports that the signer applies its own policy first.
	PolicesPayloads() bool
}

// Host-call handshake keys. A tool borrows the host's key or network by
// returning a session token and one request instead of a result:
//
//	sign  a base64 payload, answered with a detached signature
//	fetch a base64 request body, answered with the response body
//
// The host resumes with the same token until a result carries neither. The
// first call carries the public key under hostKeyField. The endpoint lives in
// the [HostFetcher] grant, so the tool cannot choose where its bytes go.
const (
	hostKeyField = "_hostKey"
	sessionField = "session"
	signatureKey = "signature"
	signErrorKey = "signError"
	responseKey  = "response"
	fetchErrKey  = "fetchError"
)

// hostCallReply is the part of a tool result the host reads; the rest passes
// through opaque.
type hostCallReply struct {
	Session string `json:"session"`
	Sign    *struct {
		Message string `json:"message"` // base64
	} `json:"sign"`
	Fetch *struct {
		Body string `json:"body"` // base64
	} `json:"fetch"`
}

// parseHostCall reads untrusted tool output. Output that is not JSON, or
// carries no sign or fetch block, is terminal, so a parse failure returns the
// zero value and no error.
func parseHostCall(text string) hostCallReply {
	var reply hostCallReply
	_ = json.Unmarshal([]byte(text), &reply)
	return reply
}

// injectHostKey adds pub, base64, under hostKeyField. Empty or null input
// starts from an empty object.
func injectHostKey(input json.RawMessage, pub []byte) (json.RawMessage, error) {
	obj := map[string]json.RawMessage{}
	if len(input) > 0 {
		if err := json.Unmarshal(input, &obj); err != nil {
			return nil, fault.Wrap(fault.Terminal, "extension_sign_input", err)
		}
		// A JSON null, in any whitespace, leaves the map nil, and assigning into
		// it would panic. Checking the decoded map catches every spelling.
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

// resumeSign resumes with the signature or the signing error.
func resumeSign(session string, sig []byte, signErr error) (json.RawMessage, error) {
	if signErr != nil {
		return resume(session, signErrorKey, signErr.Error())
	}
	return resume(session, signatureKey, base64.StdEncoding.EncodeToString(sig))
}

// resumeFetch resumes with the response body or the fetch error. The error
// goes to the tool so it can unwind what it created.
func resumeFetch(session string, body []byte, fetchErr error) (json.RawMessage, error) {
	if fetchErr != nil {
		return resume(session, fetchErrKey, fetchErr.Error())
	}
	return resume(session, responseKey, base64.StdEncoding.EncodeToString(body))
}

func resume(session, key, value string) (json.RawMessage, error) {
	b, err := json.Marshal(map[string]any{sessionField: session, key: value})
	if err != nil {
		return nil, fault.Wrap(fault.Terminal, "extension_hostcall_input", err)
	}
	return b, nil
}

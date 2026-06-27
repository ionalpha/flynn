// Package approval is the cryptographic authorization layer for privileged
// actions. It sits above the capability grant at the dispatch waist: the grant
// decides what a run is allowed to do at all, while approval requires a detached,
// verifiable signature from an authorized approver before the waist admits a
// specific privileged action (release a credential, run a destructive command,
// spend over a threshold, deploy, escalate autonomy). No valid signature, no
// action: the gate is fail-closed.
//
// The unit of authorization is a canonical, replay-proof Envelope: it names the
// action, the scope and principal it is bound to, a single-use nonce, a validity
// window, and the host it is valid on. An approver signs the envelope out of band
// (from a phone, a laptop, a hardware token) and the run carries the resulting
// Approval to the waist, where a Verifier checks the signature against a keyring of
// authorized approvers and enforces the binding: an approval is good for exactly
// one action, on one run, on one host, once, within its window. A captured
// approval cannot be replayed, widened to another action, or reused.
//
// The signing method is a port, not a fixed dependency: the default is Ed25519
// from the standard library (no external dependency), and any other method
// (hardware token, KMS, a wallet-based signer) plugs in behind the same Signer and
// keyring without changing the gate. High-risk actions can require a quorum of
// independent signatures (M-of-N), expressed as policy data. Every decision, grant
// or denial, is offered to a Sink so it lands on the event spine as an immutable,
// after-the-fact-verifiable record of who authorized what.
package approval

import (
	"encoding/binary"
	"encoding/json"
	"math"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/state"
)

// Envelope is the canonical, replay-proof description of one authorization. It is
// what an approver signs, so every field that scopes the authority is inside the
// signature: changing any of them invalidates the signature. It deliberately binds
// to the action name and scope, not the action's arguments, because the dispatch
// waist authorizes by action identity, not payload; a caller that needs to bind to
// a specific target sets it in Detail, which is also signed.
type Envelope struct {
	// Action is the dispatch action name being authorized (e.g. "secret.release",
	// "deploy"). An approval for one action never authorizes another.
	Action string `json:"action"`
	// Scope is the instance/project/workspace the action runs in. An approval is
	// valid only for its own scope.
	Scope state.Scope `json:"scope"`
	// Principal is the run (or actor) id the approval is bound to, so an approval
	// granted to one run cannot be used by another.
	Principal string `json:"principal"`
	// Detail is an optional target descriptor that further narrows the authority
	// (e.g. a resource id or "prod"). It is signed, so an approval for one target
	// does not authorize another. Empty means the action name and scope alone bound it.
	Detail string `json:"detail,omitempty"`
	// Nonce makes the approval single-use: the verifier records it on first use and
	// refuses it thereafter, so a captured approval cannot be replayed.
	Nonce string `json:"nonce"`
	// NotBefore is the unix-nano time before which the approval is not yet valid.
	// Zero means no lower bound.
	NotBefore int64 `json:"notBefore,omitempty"`
	// Expiry is the unix-nano time at or after which the approval is no longer
	// valid. Zero means it never expires, which a policy should avoid for anything
	// high-risk.
	Expiry int64 `json:"expiry,omitempty"`
	// Host is the host the approval is valid on, so an approval minted for one agent
	// host cannot authorize an action on another. Empty means any host.
	Host string `json:"host,omitempty"`
}

// signingBytes is the deterministic byte string an approver signs and a verifier
// checks. It is the JSON encoding of the envelope with a fixed field order (Go
// encodes struct fields in declaration order and the scope has no maps), prefixed
// with a domain tag and its length so a signature over an approval envelope can
// never be confused with a signature over any other message this project signs.
func (e Envelope) signingBytes() ([]byte, error) {
	body, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	const domain = "flynn/approval/v1\n"
	// Refuse an envelope so large the length-prefixed allocation would overflow.
	// This cannot happen for a real approval (the body is a small JSON object), but
	// bounding it keeps the size computation provably safe.
	if len(body) > math.MaxInt-len(domain)-8 {
		return nil, fault.New(fault.Terminal, "approval_envelope_too_large",
			"approval: envelope too large to encode")
	}
	out := make([]byte, 0, len(domain)+8+len(body))
	out = append(out, domain...)
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(body)))
	out = append(out, n[:]...)
	out = append(out, body...)
	return out, nil
}

// Approval is a signed Envelope: the authorization plus the identity of the key
// that signed it and the detached signature over the envelope's signing bytes. A
// run presents one (or several, for a quorum) to the waist.
type Approval struct {
	// Envelope is the authorization that was signed.
	Envelope Envelope `json:"envelope"`
	// KeyID identifies the approver's key in the verifier's keyring, so the verifier
	// knows which public key to check the signature against and the audit record
	// shows who signed.
	KeyID string `json:"keyId"`
	// Signature is the detached signature over Envelope.signingBytes().
	Signature []byte `json:"signature"`
}

// Decision is the record of one authorization check at the waist, offered to a
// Sink so it lands on the event spine. It captures the envelope that was required,
// the approvers whose signatures counted, whether the action was granted, and why
// when it was not.
type Decision struct {
	Envelope Envelope
	// KeyIDs are the distinct authorized approvers whose valid signatures counted
	// toward the requirement (empty on a denial with no valid signatures).
	KeyIDs []string
	// Granted reports whether the action was authorized.
	Granted bool
	// Reason explains a denial (empty when granted).
	Reason string
	// At is the unix-nano time of the decision, from the gate's clock.
	At int64
}

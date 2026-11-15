// Package approval requires a verifiable signature from an authorized approver
// before the dispatch waist admits a privileged action, such as releasing a
// credential, running a destructive command or deploying. It sits above the
// capability grant, which decides what a run may do at all, and fails closed:
// no valid signature, no action.
//
// An approver signs an [Envelope] out of band and the run carries the
// resulting [Approval] to the waist, where a [Verifier] checks it against a
// keyring. An approval is good for one action, on one run, on one host, once,
// within its window, so a captured one cannot be replayed or widened.
//
// The default signer is Ed25519 from the standard library; a hardware token,
// KMS or wallet signer plugs in behind [Signer] and the keyring without
// changing the gate. A [Policy] can require a quorum of independent signatures
// for high-risk actions, and every decision is offered to a [Sink] for the
// event spine.
package approval

import (
	"encoding/binary"
	"encoding/json"
	"math"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/state"
)

// Envelope is what an approver signs, so every field that scopes the authority
// is inside the signature. It binds the action name and scope and leaves out
// the action's arguments, because the waist authorizes by action identity; a
// caller that needs to bind a specific target sets Detail, which is signed.
type Envelope struct {
	// Action is the dispatch action name (e.g. "secret.release", "deploy").
	Action string `json:"action"`
	// Scope is the instance, project and workspace the approval is valid in.
	Scope state.Scope `json:"scope"`
	// Principal is the run or actor id the approval is bound to.
	Principal string `json:"principal"`
	// Detail narrows the authority to a target (e.g. a resource id or "prod").
	// Empty binds by action name and scope alone.
	Detail string `json:"detail,omitempty"`
	// Nonce makes the approval single-use: the verifier records it on first use and
	// refuses it after.
	Nonce string `json:"nonce"`
	// NotBefore is the unix-nano start of validity. Zero means no lower bound.
	NotBefore int64 `json:"notBefore,omitempty"`
	// Expiry is the unix-nano time from which the approval is invalid. Zero means
	// it never expires, which a policy should avoid for anything high-risk.
	Expiry int64 `json:"expiry,omitempty"`
	// Host is the host the approval is valid on. Empty means any host.
	Host string `json:"host,omitempty"`
}

// signingBytes is the byte string an approver signs and a verifier checks: the
// envelope's JSON (fixed field order, since Go encodes struct fields in
// declaration order and the scope has no maps), prefixed with a domain tag and
// its length so the signature cannot be confused with one over any other
// message this project signs.
func (e Envelope) signingBytes() ([]byte, error) {
	body, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	const domain = "flynn/approval/v1\n"
	// A real body is a small JSON object; the bound keeps the size arithmetic from
	// overflowing.
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

// Approval is a signed [Envelope] with the signing key's id and the detached
// signature. A run presents one, or several for a quorum, to the waist.
type Approval struct {
	Envelope Envelope `json:"envelope"`
	// KeyID names the approver's key in the verifier's keyring, and records who
	// signed.
	KeyID string `json:"keyId"`
	// Signature is the detached signature over the envelope's signing bytes.
	Signature []byte `json:"signature"`
}

// Decision records one authorization check at the waist, for a [Sink].
type Decision struct {
	Envelope Envelope
	// KeyIDs are the distinct authorized approvers whose valid signatures counted.
	KeyIDs []string

	Granted bool
	// Reason explains a denial; empty when granted.
	Reason string
	// At is the unix-nano time of the decision, from the gate's clock.
	At int64
}

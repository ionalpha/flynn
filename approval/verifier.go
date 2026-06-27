package approval

import (
	"context"
	"fmt"
	"sync"

	"github.com/ionalpha/flynn/clock"
)

// Verifier checks presented approvals against a required authorization. It holds
// the keyring of authorized approvers, the nonce store that enforces single use,
// the host the agent runs on, and a clock for the validity window. It is the
// trusted core of the layer: the gate asks it whether an action is authorized, and
// it answers only yes when enough independent, in-window, correctly-bound,
// not-yet-replayed signatures from authorized keys are present.
type Verifier struct {
	keyring *Keyring
	nonces  NonceStore
	clk     clock.Clock
	host    string

	// mu serializes Check so the peek-then-commit on nonces is atomic across
	// concurrent authorizations: two runs cannot both pass on the same single-use
	// approval.
	mu sync.Mutex
}

// VerifierOption configures a Verifier.
type VerifierOption func(*Verifier)

// WithClock sets the time source the validity window is checked against (default:
// clock.System).
func WithClock(c clock.Clock) VerifierOption { return func(v *Verifier) { v.clk = c } }

// WithHost sets the host id an approval must be valid on. An approval whose
// envelope names a different host is refused; an approval with an empty host is
// valid on any host. Default is the empty host (any).
func WithHost(host string) VerifierOption { return func(v *Verifier) { v.host = host } }

// NewVerifier builds a verifier over a keyring and nonce store.
func NewVerifier(keyring *Keyring, nonces NonceStore, opts ...VerifierOption) *Verifier {
	v := &Verifier{keyring: keyring, nonces: nonces, clk: clock.System{}}
	for _, o := range opts {
		o(v)
	}
	return v
}

// Check decides whether the presented approvals authorize want, requiring at least
// `required` valid signatures from distinct authorized approvers. It returns a
// Decision describing the outcome (for the audit record) and, when authorized,
// commits the contributing nonces so they cannot be replayed. It commits nothing
// unless the quorum is met, so a partial attempt does not burn a valid approval.
//
// An approval counts only when every binding holds: its envelope matches want on
// action, scope, principal, and detail; its host is this host or unset; it is
// inside its validity window; its signature verifies against an authorized key; and
// its nonce has not been spent. Signatures from the same key id count once, so one
// approver cannot satisfy a multi-signature requirement alone.
func (v *Verifier) Check(ctx context.Context, want Envelope, presented []Approval, required int) (Decision, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	now := v.clk.Now().UnixNano()
	dec := Decision{Envelope: want, At: now}

	if required <= 0 {
		dec.Granted = true
		return dec, nil
	}

	// One valid approval per distinct key id, so a quorum needs distinct approvers.
	// Remember each contributor's nonce so a met quorum commits exactly those.
	contributors := map[string]string{} // keyID -> nonce
	for _, a := range presented {
		ok, err := v.eligible(ctx, want, a, now)
		if err != nil {
			return dec, err
		}
		if !ok {
			continue
		}
		if _, dup := contributors[a.KeyID]; dup {
			continue
		}
		contributors[a.KeyID] = a.Envelope.Nonce
	}

	if len(contributors) < required {
		dec.Reason = insufficientReason(len(contributors), required, len(presented))
		return dec, nil
	}

	// Quorum met: commit the contributing nonces. A commit that loses a race (the
	// nonce was spent between the peek and here) drops that contributor; if that
	// pushes the count below the requirement, the action is denied rather than
	// admitted on a replayed approval.
	committed := make([]string, 0, len(contributors))
	for keyID, nonce := range contributors {
		if err := v.nonces.Use(ctx, nonce); err != nil {
			continue
		}
		committed = append(committed, keyID)
	}
	if len(committed) < required {
		dec.Reason = "approval: a contributing nonce was spent concurrently; quorum no longer met"
		return dec, nil
	}

	dec.Granted = true
	dec.KeyIDs = committed
	return dec, nil
}

// eligible reports whether a single approval is a valid, in-window, correctly-bound
// signature from an authorized, not-yet-spent key for want. It does not commit the
// nonce; Check commits only once a quorum is confirmed.
func (v *Verifier) eligible(ctx context.Context, want Envelope, a Approval, now int64) (bool, error) {
	e := a.Envelope
	// Binding: an approval authorizes exactly its own action, scope, principal, and
	// target. A mismatch means this approval is for something else.
	if e.Action != want.Action || e.Scope != want.Scope || e.Principal != want.Principal || e.Detail != want.Detail {
		return false, nil
	}
	// Host: valid on this host, or host-agnostic when unset.
	if e.Host != "" && e.Host != v.host {
		return false, nil
	}
	// Window: not before NotBefore, not at or after Expiry.
	if e.NotBefore != 0 && now < e.NotBefore {
		return false, nil
	}
	if e.Expiry != 0 && now >= e.Expiry {
		return false, nil
	}
	// Signature: a real signature from an authorized key.
	msg, err := e.signingBytes()
	if err != nil {
		return false, err
	}
	if !v.keyring.verify(a.KeyID, msg, a.Signature) {
		return false, nil
	}
	// Replay: refuse a nonce already spent (peek only; Check commits later).
	seen, err := v.nonces.Seen(ctx, e.Nonce)
	if err != nil {
		return false, err
	}
	return !seen, nil
}

// insufficientReason explains why a quorum was not met, for the audit record.
func insufficientReason(have, required, presented int) string {
	if presented == 0 {
		return "approval: no approvals presented for a privileged action"
	}
	return fmt.Sprintf("approval: %d of %d required signatures valid (%d presented)", have, required, presented)
}

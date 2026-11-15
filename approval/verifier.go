package approval

import (
	"context"
	"fmt"
	"sync"

	"github.com/ionalpha/flynn/clock"
)

// Verifier decides whether presented approvals authorize an action. It answers
// yes only when enough independent, in-window, correctly bound, unspent
// signatures from keys in its keyring are present.
//
//flynn:policy
type Verifier struct {
	keyring *Keyring
	nonces  NonceStore
	clk     clock.Clock
	host    string

	// mu serializes Check so the nonce peek-then-commit is atomic: two runs cannot
	// both pass on the same single-use approval.
	mu sync.Mutex
}

// VerifierOption configures a [Verifier] at construction.
type VerifierOption func(*Verifier)

// WithClock sets the clock the validity window is checked against. Default:
// clock.System.
func WithClock(c clock.Clock) VerifierOption { return func(v *Verifier) { v.clk = c } }

// WithHost sets the host an approval must be valid on. An envelope naming
// another host is refused; one with an empty host is valid anywhere. Default:
// empty (any host).
func WithHost(host string) VerifierOption { return func(v *Verifier) { v.host = host } }

// NewVerifier verifies against keyring and spends nonces in nonces.
func NewVerifier(keyring *Keyring, nonces NonceStore, opts ...VerifierOption) *Verifier {
	v := &Verifier{keyring: keyring, nonces: nonces, clk: clock.System{}}
	for _, o := range opts {
		o(v)
	}
	return v
}

// Check reports whether presented holds at least required valid signatures
// from distinct authorized keys for want. It commits the contributing nonces
// only when the quorum is met, so a partial attempt does not burn a valid
// approval.
//
// An approval counts only when its envelope matches want on action, scope,
// principal and detail; its host is this host or unset; it is inside its
// window; its signature verifies; and its nonce is unspent. One key id counts
// once.
//
// Zero authorizes without a signature. A negative count is corrupt and is
// denied, because reading it as zero would admit the action.
func (v *Verifier) Check(ctx context.Context, want Envelope, presented []Approval, required int) (Decision, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	now := v.clk.Now().UnixNano()
	dec := Decision{Envelope: want, At: now}

	if required < 0 {
		dec.Reason = "approval: a negative signature requirement is not a policy; refusing"
		return dec, nil
	}
	if required == 0 {
		dec.Granted = true
		return dec, nil
	}

	// One approval per key id, so a quorum needs distinct approvers. Each
	// contributor's nonce is kept so a met quorum commits exactly those.
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

	// A commit that loses a race (spent since the peek) drops that contributor; if
	// the count falls below the requirement the action is denied, so a replayed
	// approval is never admitted.
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

// eligible reports whether a counts toward want. It only peeks at the nonce;
// Check commits once a quorum is confirmed.
func (v *Verifier) eligible(ctx context.Context, want Envelope, a Approval, now int64) (bool, error) {
	e := a.Envelope

	if e.Action != want.Action || e.Scope != want.Scope || e.Principal != want.Principal || e.Detail != want.Detail {
		return false, nil
	}
	// An empty host is valid on any host.
	if e.Host != "" && e.Host != v.host {
		return false, nil
	}

	if e.NotBefore != 0 && now < e.NotBefore {
		return false, nil
	}
	if e.Expiry != 0 && now >= e.Expiry {
		return false, nil
	}

	msg, err := e.signingBytes()
	if err != nil {
		return false, err
	}
	if !v.keyring.verify(a.KeyID, msg, a.Signature) {
		return false, nil
	}
	// Peek only; Check commits later.
	seen, err := v.nonces.Seen(ctx, e.Nonce)
	if err != nil {
		return false, err
	}
	return !seen, nil
}

func insufficientReason(have, required, presented int) string {
	if presented == 0 {
		return "approval: no approvals presented for a privileged action"
	}
	return fmt.Sprintf("approval: %d of %d required signatures valid (%d presented)", have, required, presented)
}

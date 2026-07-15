package extension

import (
	"context"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/secret"
)

// RoutedSigner is a HostSigner whose key lives in ANOTHER extension: a signer extension,
// holding one chain's key and that chain's transaction parser.
//
// This is the shape that lets the host stay ignorant. A host that holds a key cannot sign
// safely without understanding what it signs, so a host that holds a key must carry a parser
// for every format it might be asked to sign, and it stops being a general engine the moment
// it does. Move the key out and the parser goes with it: the host is left routing opaque
// bytes between two processes and reading none of them.
//
// The separation is also what makes the check honest. The worker extension BUILDS the
// transaction; the signer HOLDS the key and independently decides whether to sign it. They
// are different artifacts, published and pinned separately. A worker compromised upstream
// gets no signature: it would have to compromise the signer too, and the signer is small
// enough to audit line by line and does nothing but parse, decide, and sign.
//
// What must never happen is a worker that carries its own parser and vouches for its own
// payload. That is self-policing, and it buys exactly nothing.
//
// The signer is reached over a SignerChannel, which is the host's private line to it, NOT the
// agent's tool surface. Its tools are never mounted, so the model can neither unlock the key
// nor ask for a signature behind the worker's back.
type RoutedSigner struct {
	ch  SignerChannel
	pub []byte
}

// NewRoutedSigner unlocks a signer extension and returns a signer that routes signing requests
// to it. It fails if the signer cannot be reached, refuses the passphrase, or does not answer
// with a key, so a worker is never mounted against a signer that cannot sign: the failure lands
// at mount, where an operator sees it, rather than halfway through a mint.
//
// The passphrase is the operator's, held in the host's vault. The host never learns the key it
// unlocks: what comes back is the public half.
//
// keyPath names the sealed key on this machine. A released signer is launched from a catalog
// spec whose arguments were fixed before the machine existed, so it cannot have been told the
// path as a flag; the host names it here instead. The path is not a secret (the signer, not the
// host, opens the file), and a signer already launched with its own --key ignores it. Empty when
// the operator dev-linked a signer that carries its own key.
func NewRoutedSigner(ctx context.Context, ch SignerChannel, passphrase secret.Text, keyPath string) (*RoutedSigner, error) {
	if ch == nil {
		return nil, fault.New(fault.Terminal, "extension_signer_missing",
			"extension: no channel to a signer extension, so there is nothing to sign with")
	}
	pub, _, err := ch.Unlock(ctx, passphrase, keyPath)
	if err != nil {
		return nil, err
	}
	return &RoutedSigner{ch: ch, pub: pub}, nil
}

// Public returns the signer's public key, as the signer reported it at unlock.
func (s *RoutedSigner) Public() []byte { return s.pub }

// Sign hands the payload to the signer extension and returns the detached signature it
// produced. The signer may refuse, in which case its refusal is returned as-is: it names the
// rule the payload broke, and that reason belongs to the operator reading the error, not to
// this host, which cannot check the claim and does not try.
func (s *RoutedSigner) Sign(ctx context.Context, payload []byte) ([]byte, error) {
	return s.ch.SignPayload(ctx, payload)
}

// PolicesPayloads reports that the signer applies its own policy. It holds the key and it
// understands the format, so it is the only component positioned to judge the payload, and
// the host requires no policy of its own for it.
func (s *RoutedSigner) PolicesPayloads() bool { return true }

var (
	_ HostSigner   = (*RoutedSigner)(nil)
	_ SelfPolicing = (*RoutedSigner)(nil)
)

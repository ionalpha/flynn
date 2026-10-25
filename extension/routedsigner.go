package extension

import (
	"context"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/secret"
)

// RoutedSigner is a HostSigner whose key lives in ANOTHER extension: a signer extension,
// holding one chain's key and that chain's transaction parser.
//
// A host holding a key cannot sign safely without understanding what it signs, and would
// need a parser per format. Moving the key out moves the parser with it: the host only
// routes opaque bytes between two processes.
//
// It also keeps the check independent. The worker BUILDS the transaction; the signer HOLDS
// the key and decides alone whether to sign. They are pinned separately, so a compromised
// worker gets no signature unless the signer is too; a self-vouching worker checks nothing.
//
// The signer is reached over a SignerChannel, the host's private line, not the agent's
// tool surface. Its tools are never mounted, so the model can neither unlock the key nor
// ask for a signature behind the worker's back.
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

package extension

import (
	"context"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/secret"
)

// RoutedSigner is a [HostSigner] whose key and transaction parser live in a
// signer extension, so the host routes opaque bytes and needs no parser. The
// worker builds and the separately pinned signer decides, so a compromised
// worker gets no signature on its own. The signer is reached over a
// [SignerChannel] and its tools are never mounted for the model.
type RoutedSigner struct {
	ch  SignerChannel
	pub []byte
}

// NewRoutedSigner unlocks the signer and fails at mount, where the operator
// sees it, if no key comes back. Only the public half returns to the host.
// keyPath names the sealed key, since a released signer's args were fixed in
// the catalog; it is no secret, is ignored by a signer with its own --key,
// and is empty for a dev-linked signer.
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

// Public returns the public key reported at unlock.
func (s *RoutedSigner) Public() []byte { return s.pub }

// Sign returns the signer's detached signature, or its refusal unchanged for
// the operator to read.
func (s *RoutedSigner) Sign(ctx context.Context, payload []byte) ([]byte, error) {
	return s.ch.SignPayload(ctx, payload)
}

// PolicesPayloads reports true: the signer holds the key and parses the
// format.
func (s *RoutedSigner) PolicesPayloads() bool { return true }

var (
	_ HostSigner   = (*RoutedSigner)(nil)
	_ SelfPolicing = (*RoutedSigner)(nil)
)

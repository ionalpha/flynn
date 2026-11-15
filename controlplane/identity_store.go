package controlplane

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/ionalpha/flynn/secret"
)

// IdentityVaultRef is the vault reference the instance identity seed is sealed under.
const IdentityVaultRef = "controlplane/instance-identity"

// SeedVault is the subset of *vault.Store needed to persist an identity, so this
// package does not import the vault backend and a test can use a map.
type SeedVault interface {
	// Lookup returns secret.ErrNotFound for a ref that was never sealed. That is the
	// signal LoadOrCreateIdentity reads as "first run" before it mints a new seed, so an
	// implementation must not collapse absence into a generic error.
	Lookup(ctx context.Context, ref string) (secret.Text, error)
	Set(ctx context.Context, ref string, value secret.Text) error
}

// Seed returns the 32-byte Ed25519 seed the keypair derives from. Holding it is
// holding the private key: never log it or store it unsealed.
func (i *Identity) Seed() []byte {
	return i.priv.Seed()
}

// IdentityFromSeed rebuilds an identity from a 32-byte seed. Any other length is
// rejected, so a corrupt sealed value fails instead of yielding a wrong identity.
func IdentityFromSeed(seed []byte) (*Identity, error) {
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("controlplane: identity seed must be %d bytes, got %d", ed25519.SeedSize, len(seed))
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("controlplane: identity from seed: unexpected key type")
	}
	return &Identity{pub: pub, priv: priv}, nil
}

// LoadOrCreateIdentity returns an identity that is stable across restarts, so tokens
// issued to it keep verifying and audit keeps one principal. It loads the seed sealed
// under ref, or on first run generates one and seals it before returning. The seed
// travels as a secret.Text, so it is redacted and never logged. A present but corrupt
// seed is an error rather than a reason to mint a new identity and orphan every token
// issued to the old one.
func LoadOrCreateIdentity(ctx context.Context, v SeedVault, ref string) (*Identity, error) {
	if v == nil {
		return nil, errors.New("controlplane: load identity: nil vault")
	}
	if ref == "" {
		ref = IdentityVaultRef
	}
	sealed, err := v.Lookup(ctx, ref)
	switch {
	case err == nil:
		seed, derr := base64.RawURLEncoding.DecodeString(sealed.Expose())
		if derr != nil {
			return nil, fmt.Errorf("controlplane: load identity: decode sealed seed: %w", derr)
		}
		id, ierr := IdentityFromSeed(seed)
		if ierr != nil {
			return nil, fmt.Errorf("controlplane: load identity: %w", ierr)
		}
		return id, nil
	case errors.Is(err, secret.ErrNotFound):
		// First run: mint and seal, so the next restart loads this same identity.
		id, gerr := GenerateIdentity()
		if gerr != nil {
			return nil, gerr
		}
		enc := base64.RawURLEncoding.EncodeToString(id.Seed())
		if serr := v.Set(ctx, ref, secret.New(enc)); serr != nil {
			return nil, fmt.Errorf("controlplane: persist identity: %w", serr)
		}
		return id, nil
	default:
		return nil, fmt.Errorf("controlplane: load identity: %w", err)
	}
}

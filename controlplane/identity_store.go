package controlplane

// Persistent instance identity. The keystone token treats an Identity's public key as
// the instance's verifiable id, but a freshly-generated-per-process key means every
// restart is a new principal: tokens issued to the old key stop verifying, and audit
// loses the thread across restarts. This file seals the Ed25519 seed in the existing
// vault so identity is stable at rest. The seed (32 bytes, base64) is the only secret;
// the public key and id derive from it, so persisting the seed persists the identity.
//
// The vault is reached through a narrow SeedVault port (the subset of vault.Store this
// needs), not by importing vault: the cryptographic core stays free of the credential
// backend, and a test supplies an in-memory map. The seed is carried as a secret.Text
// so it inherits the vault's redaction and sealed-at-rest handling and is never logged.

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

// SeedVault is the slice of a credential vault this package needs to persist an
// identity: read a sealed value, write one. It is exactly satisfied by *vault.Store, so
// the production wiring passes the real vault while a test passes an in-memory stub, and
// controlplane never has to import the vault backend.
type SeedVault interface {
	Lookup(ctx context.Context, ref string) (secret.Text, error)
	Set(ctx context.Context, ref string, value secret.Text) error
}

// Seed returns the identity's 32-byte Ed25519 seed, the single secret the rest of the
// keypair derives from. It is the value LoadOrCreateIdentity seals in the vault. Holding
// it is equivalent to holding the private key, so callers must treat it as a secret and
// never log or persist it unsealed.
func (i *Identity) Seed() []byte {
	return i.priv.Seed()
}

// IdentityFromSeed reconstructs an identity from a 32-byte Ed25519 seed, the inverse of
// Seed. A seed of the wrong length is rejected rather than silently truncated, so a
// corrupted or foreign sealed value fails closed instead of yielding a usable-but-wrong
// identity.
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

// LoadOrCreateIdentity returns the instance's stable identity: it reads the sealed seed
// from the vault and reconstructs the keypair, or, on first run (no sealed seed yet),
// generates a fresh identity and seals its seed before returning it. Either way the
// returned identity is the same one a later restart will load, so the instance's public
// key and principal id survive restarts rather than churning per process.
//
// It fails closed on a present-but-corrupt seed (a wrong length or undecodable value):
// rather than silently minting a new identity and orphaning every token issued to the
// old key, it surfaces the error so the operator notices a tampered or truncated vault.
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

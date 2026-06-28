package controlplane

import (
	"context"
	"errors"
	"testing"

	"github.com/ionalpha/flynn/secret"
)

// memVault is an in-memory SeedVault for tests: it stands in for the sealed credential
// vault so identity persistence can be exercised without a real keychain or file.
type memVault struct {
	m map[string]string
}

func newMemVault() *memVault { return &memVault{m: map[string]string{}} }

func (v *memVault) Lookup(_ context.Context, ref string) (secret.Text, error) {
	val, ok := v.m[ref]
	if !ok {
		return secret.Text{}, secret.ErrNotFound
	}
	return secret.New(val), nil
}

func (v *memVault) Set(_ context.Context, ref string, value secret.Text) error {
	v.m[ref] = value.Expose()
	return nil
}

func TestSeedRoundTrip(t *testing.T) {
	id := dgIdentity(t)
	back, err := IdentityFromSeed(id.Seed())
	if err != nil {
		t.Fatalf("from seed: %v", err)
	}
	if back.ID() != id.ID() {
		t.Fatalf("reconstructed id = %q, want %q", back.ID(), id.ID())
	}
	if !back.Public().Equal(id.Public()) {
		t.Fatal("reconstructed public key differs from original")
	}
}

func TestIdentityFromSeedRejectsWrongLength(t *testing.T) {
	if _, err := IdentityFromSeed([]byte("too short")); err == nil {
		t.Fatal("expected an error for a short seed")
	}
}

// Identity must survive a restart: a second load from the same vault yields the same
// principal id and key, not a freshly generated one.
func TestLoadOrCreateIdentityStableAcrossRestart(t *testing.T) {
	ctx := context.Background()
	v := newMemVault()

	first, err := LoadOrCreateIdentity(ctx, v, "")
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	second, err := LoadOrCreateIdentity(ctx, v, "")
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	if first.ID() != second.ID() {
		t.Fatalf("identity changed across restart: %q != %q", first.ID(), second.ID())
	}
	if !first.Public().Equal(second.Public()) {
		t.Fatal("public key changed across restart")
	}
}

// A present-but-corrupt sealed seed must fail closed, not silently mint a new identity
// and orphan every token issued to the old key.
func TestLoadOrCreateIdentityRejectsCorruptSeed(t *testing.T) {
	ctx := context.Background()
	v := newMemVault()
	v.m[IdentityVaultRef] = "!!!not-base64!!!"
	if _, err := LoadOrCreateIdentity(ctx, v, ""); err == nil {
		t.Fatal("expected an error for a corrupt sealed seed")
	}

	v.m[IdentityVaultRef] = "c2hvcnQ" // valid base64, but not 32 bytes
	if _, err := LoadOrCreateIdentity(ctx, v, ""); err == nil {
		t.Fatal("expected an error for a wrong-length sealed seed")
	}
}

func TestLoadOrCreateIdentityNilVault(t *testing.T) {
	if _, err := LoadOrCreateIdentity(context.Background(), nil, ""); err == nil {
		t.Fatal("expected an error for a nil vault")
	}
}

// A vault read error (neither success nor ErrNotFound) must surface, not be mistaken for
// first-run and quietly overwrite a possibly-present identity.
func TestLoadOrCreateIdentityPropagatesReadError(t *testing.T) {
	boom := errors.New("vault unavailable")
	v := errVault{err: boom}
	if _, err := LoadOrCreateIdentity(context.Background(), v, ""); !errors.Is(err, boom) {
		t.Fatalf("error = %v, want it to wrap %v", err, boom)
	}
}

type errVault struct{ err error }

func (e errVault) Lookup(context.Context, string) (secret.Text, error) {
	return secret.Text{}, e.err
}
func (e errVault) Set(context.Context, string, secret.Text) error { return e.err }

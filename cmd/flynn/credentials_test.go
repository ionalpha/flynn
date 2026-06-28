package main

import (
	"context"
	"errors"
	"testing"

	"github.com/ionalpha/flynn/credential"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/secret"
	"github.com/ionalpha/flynn/vault"
)

// memKeyring is an in-memory vault.Keyring for tests, so the vault uses its
// keychain path without touching the host keychain.
type memKeyring map[string]string

func (m memKeyring) Get(_, key string) (string, error) {
	if v, ok := m[key]; ok {
		return v, nil
	}
	return "", vault.ErrKeyNotFound
}
func (m memKeyring) Set(_, key, val string) error { m[key] = val; return nil }
func (m memKeyring) Delete(_, key string) error   { delete(m, key); return nil }

func testStores(t *testing.T) (*credential.Store, *vault.Store) {
	t.Helper()
	reg := resource.NewRegistry()
	if err := credential.RegisterKind(reg); err != nil {
		t.Fatalf("register kind: %v", err)
	}
	cs := credential.NewStore(resource.NewMemory(reg))
	vs := vault.New(t.TempDir(), vault.WithKeyring(memKeyring{}))
	return cs, vs
}

func TestAddCredentialStoresMetadataAndSecret(t *testing.T) {
	cs, vs := testStores(t)
	ctx := context.Background()
	spec := credential.Spec{Integration: "cf", Name: "prod", AuthType: "bearer", Role: credential.RoleOperator, IsDefault: true}
	if err := addCredential(ctx, cs, vs, spec, secret.New("tok-123")); err != nil {
		t.Fatalf("add: %v", err)
	}
	// Metadata is recorded.
	got, err := cs.Get(ctx, "cf", "prod")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Spec.Role != credential.RoleOperator || !got.Spec.IsDefault {
		t.Fatalf("metadata: %+v", got.Spec)
	}
	// The secret is in the vault at the credential's reference.
	v, err := vs.Lookup(ctx, "cf/prod")
	if err != nil {
		t.Fatalf("vault lookup: %v", err)
	}
	if v.Expose() != "tok-123" {
		t.Fatalf("vault value: %q", v.Expose())
	}
}

// TestAddCredentialFirstIsDefault proves the first credential for an integration
// becomes its default automatically, so a single credential is usable without a
// separate selection step, while a later credential does not steal the default.
func TestAddCredentialFirstIsDefault(t *testing.T) {
	cs, vs := testStores(t)
	ctx := context.Background()
	if err := addCredential(ctx, cs, vs, credential.Spec{Integration: "cf", Name: "a"}, secret.New("1")); err != nil {
		t.Fatalf("add first: %v", err)
	}
	def, err := cs.Default(ctx, "cf")
	if err != nil || def.Spec.Name != "a" {
		t.Fatalf("the first credential should be the default, got %+v (%v)", def, err)
	}
	if err := addCredential(ctx, cs, vs, credential.Spec{Integration: "cf", Name: "b"}, secret.New("2")); err != nil {
		t.Fatalf("add second: %v", err)
	}
	def, _ = cs.Default(ctx, "cf")
	if def.Spec.Name != "a" {
		t.Fatalf("a later credential must not steal the default, got %q", def.Spec.Name)
	}
}

func TestAddCredentialRollsBackOnMetadataFailure(t *testing.T) {
	cs, vs := testStores(t)
	ctx := context.Background()
	// A slash in the name is rejected by the credential store, so Put fails after the
	// vault write; the write must be rolled back.
	spec := credential.Spec{Integration: "cf", Name: "bad/name"}
	if err := addCredential(ctx, cs, vs, spec, secret.New("secret")); err == nil {
		t.Fatal("expected the invalid credential to be rejected")
	}
	if _, err := vs.Lookup(ctx, "cf/bad/name"); !errors.Is(err, secret.ErrNotFound) {
		t.Fatalf("the vault write should have been rolled back, got %v", err)
	}
}

func TestRemoveCredentialDeletesBoth(t *testing.T) {
	cs, vs := testStores(t)
	ctx := context.Background()
	spec := credential.Spec{Integration: "cf", Name: "gone"}
	if err := addCredential(ctx, cs, vs, spec, secret.New("v")); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := removeCredential(ctx, cs, vs, "cf", "gone"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := cs.Get(ctx, "cf", "gone"); !errors.Is(err, credential.ErrNotFound) {
		t.Fatalf("metadata should be gone, got %v", err)
	}
	if _, err := vs.Lookup(ctx, "cf/gone"); !errors.Is(err, secret.ErrNotFound) {
		t.Fatalf("vault secret should be gone, got %v", err)
	}
}

func TestRemoveMissingCredential(t *testing.T) {
	cs, vs := testStores(t)
	if err := removeCredential(context.Background(), cs, vs, "cf", "nope"); !errors.Is(err, credential.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// TestCredentialCLIRoundTrip exercises the real durable store path used by the CLI:
// add two credentials, confirm the default, then remove one.
func TestCredentialCLIRoundTrip(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	vs := vault.New(dataDir, vault.WithKeyring(memKeyring{}))

	cs, closeStore, err := openCredentialStore(ctx, dataDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = closeStore() }()

	if err := addCredential(ctx, cs, vs, credential.Spec{Integration: "cf", Name: "prod", IsDefault: true}, secret.New("p")); err != nil {
		t.Fatalf("add prod: %v", err)
	}
	if err := addCredential(ctx, cs, vs, credential.Spec{Integration: "cf", Name: "staging"}, secret.New("s")); err != nil {
		t.Fatalf("add staging: %v", err)
	}

	creds, err := cs.List(ctx, "cf")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(creds) != 2 {
		t.Fatalf("expected 2 credentials, got %d", len(creds))
	}
	def, err := cs.Default(ctx, "cf")
	if err != nil || def.Spec.Name != "prod" {
		t.Fatalf("default: %v %+v", err, def)
	}

	if err := removeCredential(ctx, cs, vs, "cf", "staging"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	creds, _ = cs.List(ctx, "cf")
	if len(creds) != 1 || creds[0].Spec.Name != "prod" {
		t.Fatalf("after remove: %+v", creds)
	}
}

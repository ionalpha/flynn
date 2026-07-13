package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/internal/credential"
	"github.com/ionalpha/flynn/internal/vault"
	"github.com/ionalpha/flynn/secret"
)

// fixedPrompt answers the secret prompt with a value, standing in for the no-echo
// terminal read the command uses.
func fixedPrompt(value string) secretPrompt {
	return func(string) (secret.Text, error) { return secret.New(value), nil }
}

// failingPrompt stands in for a prompt that could not be read (no terminal, or the
// operator interrupted it).
func failingPrompt(err error) secretPrompt {
	return func(string) (secret.Text, error) { return secret.Text{}, err }
}

// TestAuthCredAddStoresTheSecretAndTheMetadata is the point of the command: after it
// runs, the metadata is in the resource store and the value is sealed in the vault, and
// the value was never written to the store.
func TestAuthCredAddStoresTheSecretAndTheMetadata(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	vs := vault.New(dataDir, vault.WithKeyring(memKeyring{}))

	args := []string{"cloudflare", "--name", "prod", "--role", "operator", "--type", "bearer", "--default"}
	if err := authCredAdd(ctx, vs, dataDir, args, fixedPrompt("tok-abc")); err != nil {
		t.Fatalf("auth add: %v", err)
	}

	cs, closeStore, err := openCredentialStore(ctx, dataDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = closeStore() }()

	got, err := cs.Get(ctx, "cloudflare", "prod")
	if err != nil {
		t.Fatalf("the credential metadata was not recorded: %v", err)
	}
	if got.Spec.Role != credential.RoleOperator || got.Spec.AuthType != "bearer" || !got.Spec.IsDefault {
		t.Fatalf("metadata = %+v, want the flags honoured", got.Spec)
	}
	v, err := vs.Lookup(ctx, credential.VaultRef("cloudflare", "prod"))
	if err != nil {
		t.Fatalf("the secret is not in the vault: %v", err)
	}
	if v.Expose() != "tok-abc" {
		t.Fatalf("vault value = %q, want the value that was entered", v.Expose())
	}
}

// TestAuthCredAddDefaultsTheName: a single credential per integration needs no --name.
func TestAuthCredAddDefaultsTheName(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	vs := vault.New(dataDir, vault.WithKeyring(memKeyring{}))

	if err := authCredAdd(ctx, vs, dataDir, []string{"stripe"}, fixedPrompt("sk")); err != nil {
		t.Fatalf("auth add: %v", err)
	}
	cs, closeStore, err := openCredentialStore(ctx, dataDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = closeStore() }()
	if _, err := cs.Get(ctx, "stripe", "default"); err != nil {
		t.Fatalf("the credential was not stored under the default name: %v", err)
	}
}

// TestAuthCredAddRefusesBadInput: every rejection happens before the vault is written, so
// a refused add cannot leave a secret behind with no credential pointing at it.
func TestAuthCredAddRefusesBadInput(t *testing.T) {
	cases := map[string]struct {
		args   []string
		prompt secretPrompt
		want   string
	}{
		"no integration":       {nil, fixedPrompt("v"), "usage"},
		"flag before the name": {[]string{"--name", "prod"}, fixedPrompt("v"), "usage"},
		"unknown flag":         {[]string{"cf", "--no-such-flag"}, fixedPrompt("v"), "flag provided"},
		"stray positional":     {[]string{"cf", "extra"}, fixedPrompt("v"), "unexpected arguments"},
		"empty name":           {[]string{"cf", "--name", ""}, fixedPrompt("v"), "must not be empty"},
		"unknown role":         {[]string{"cf", "--role", "superuser"}, fixedPrompt("v"), "unknown role"},
		"no secret entered":    {[]string{"cf"}, fixedPrompt(""), "no secret entered"},
		"prompt failed":        {[]string{"cf"}, failingPrompt(errors.New("no terminal")), "no terminal"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			dataDir := t.TempDir()
			vs := vault.New(dataDir, vault.WithKeyring(memKeyring{}))
			if err := authCredAdd(ctx, vs, dataDir, tc.args, tc.prompt); err == nil {
				t.Fatal("expected the add to be refused")
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.want)
			}
			if _, err := vs.Lookup(ctx, credential.VaultRef("cf", "default")); !errors.Is(err, secret.ErrNotFound) {
				t.Fatalf("a refused add wrote a secret to the vault (err = %v)", err)
			}
		})
	}
}

// TestAuthCredUseSwitchesTheDefault: `auth use` is how a bare integration reference is
// pointed at another credential.
func TestAuthCredUseSwitchesTheDefault(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	vs := vault.New(dataDir, vault.WithKeyring(memKeyring{}))

	for _, name := range []string{"prod", "staging"} {
		if err := authCredAdd(ctx, vs, dataDir, []string{"cf", "--name", name}, fixedPrompt(name)); err != nil {
			t.Fatalf("add %s: %v", name, err)
		}
	}
	if err := authCredUse(ctx, dataDir, []string{"cf/staging"}); err != nil {
		t.Fatalf("auth use: %v", err)
	}

	cs, closeStore, err := openCredentialStore(ctx, dataDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = closeStore() }()
	def, err := cs.Default(ctx, "cf")
	if err != nil {
		t.Fatalf("default: %v", err)
	}
	if def.Spec.Name != "staging" {
		t.Fatalf("default = %q, want the one that was selected", def.Spec.Name)
	}
}

// TestAuthCredUseRefusesBadInput: the reference must name both halves, so a bare
// integration cannot silently select nothing.
func TestAuthCredUseRefusesBadInput(t *testing.T) {
	dataDir := t.TempDir()
	for name, args := range map[string][]string{
		"no reference":       {},
		"two references":     {"cf/a", "cf/b"},
		"no name":            {"cf"},
		"empty name":         {"cf/"},
		"empty integration":  {"/prod"},
		"unknown credential": {"cf/nosuchname"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := authCredUse(context.Background(), dataDir, args); err == nil {
				t.Fatal("expected the selection to be refused")
			}
		})
	}
}

// TestAuthCredListShowsRoleAndDefault: the listing is how an operator sees which
// credential a bare integration reference resolves to.
func TestAuthCredListShowsRoleAndDefault(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	vs := vault.New(dataDir, vault.WithKeyring(memKeyring{}))

	var empty bytes.Buffer
	if err := authCredList(ctx, dataDir, "cf", &empty); err != nil {
		t.Fatalf("ls on an integration with no credentials: %v", err)
	}
	if !strings.Contains(empty.String(), "no credentials for cf") {
		t.Fatalf("output = %q, want it to say there are none", empty.String())
	}

	if err := authCredAdd(ctx, vs, dataDir, []string{"cf", "--name", "prod", "--role", "admin", "--type", "bearer"}, fixedPrompt("p")); err != nil {
		t.Fatalf("add prod: %v", err)
	}
	if err := authCredAdd(ctx, vs, dataDir, []string{"cf", "--name", "staging"}, fixedPrompt("s")); err != nil {
		t.Fatalf("add staging: %v", err)
	}

	var buf bytes.Buffer
	if err := authCredList(ctx, dataDir, "cf", &buf); err != nil {
		t.Fatalf("ls: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"NAME", "ROLE", "DEFAULT", "prod", "admin", "bearer", "staging"} {
		if !strings.Contains(out, want) {
			t.Errorf("the listing is missing %q:\n%s", want, out)
		}
	}
	// prod was added first, so it is the default and is the row that carries the marker.
	if !strings.Contains(lineContaining(out, "prod"), "*") {
		t.Errorf("the default credential is not marked:\n%s", out)
	}
	if strings.Contains(lineContaining(out, "staging"), "*") {
		t.Errorf("a non-default credential is marked as the default:\n%s", out)
	}
	// A credential with neither a role nor a type renders as "-", never as a blank column.
	if !strings.Contains(lineContaining(out, "staging"), "-") {
		t.Errorf("an unset role and type must render as a dash:\n%s", out)
	}
}

// TestAuthCredRemoveDeletesBothHalves: the metadata and the vault secret go together, so
// a removed credential leaves no orphaned secret behind.
func TestAuthCredRemoveDeletesBothHalves(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	vs := vault.New(dataDir, vault.WithKeyring(memKeyring{}))

	if err := authCredAdd(ctx, vs, dataDir, []string{"cf", "--name", "prod"}, fixedPrompt("p")); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := authCredRemove(ctx, vs, dataDir, []string{"cf/prod"}); err != nil {
		t.Fatalf("rm: %v", err)
	}

	cs, closeStore, err := openCredentialStore(ctx, dataDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = closeStore() }()
	if _, err := cs.Get(ctx, "cf", "prod"); !errors.Is(err, credential.ErrNotFound) {
		t.Fatalf("the metadata survived the removal (err = %v)", err)
	}
	if _, err := vs.Lookup(ctx, credential.VaultRef("cf", "prod")); !errors.Is(err, secret.ErrNotFound) {
		t.Fatalf("the vault secret survived the removal (err = %v)", err)
	}
}

// TestAuthCredRemoveRefusesBadInput: rm takes exactly one <integration>/<name>.
func TestAuthCredRemoveRefusesBadInput(t *testing.T) {
	dataDir := t.TempDir()
	vs := vault.New(dataDir, vault.WithKeyring(memKeyring{}))
	for name, args := range map[string][]string{
		"no reference":       {},
		"two references":     {"cf/a", "cf/b"},
		"no name":            {"cf"},
		"empty name":         {"cf/"},
		"unknown credential": {"cf/nosuchname"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := authCredRemove(context.Background(), vs, dataDir, args); err == nil {
				t.Fatal("expected the removal to be refused")
			}
		})
	}
}

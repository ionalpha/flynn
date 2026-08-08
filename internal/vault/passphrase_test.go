package vault

// The passphrase read from the environment, which is the default source and the one a
// headless host has. What counts as set, what counts as absent, and what the vault does
// with each.

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/ionalpha/flynn/secret"
)

func TestEnvPassphrase(t *testing.T) {
	tests := []struct {
		name    string
		set     bool
		value   string
		want    string
		wantErr error
	}{
		{name: "unset", set: false, wantErr: ErrNoPassphrase},
		{name: "empty is treated as unset", set: true, value: "", wantErr: ErrNoPassphrase},
		{name: "present", set: true, value: "unlock-me", want: "unlock-me"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_ = os.Unsetenv("FLYNN_VAULT_PASSPHRASE")
			if tc.set {
				t.Setenv("FLYNN_VAULT_PASSPHRASE", tc.value)
			}
			got, err := EnvPassphrase(false)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("got %v, want %v", err, tc.wantErr)
				}
				if !got.Empty() {
					t.Fatalf("an errored passphrase returned a value: %q", got.Expose())
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Expose() != tc.want {
				t.Fatalf("got %q, want %q", got.Expose(), tc.want)
			}
		})
	}
}

// TestEnvPassphraseIsTheDefault proves the wiring, not just the function: a Store
// built with no WithPassphrase unlocks a sealed file from the environment, which is
// what lets a container run non-interactively.
func TestEnvPassphraseIsTheDefault(t *testing.T) {
	t.Setenv("FLYNN_VAULT_PASSPHRASE", "from-the-env")
	ctx := context.Background()
	dir := t.TempDir()

	s := New(dir, WithKeyring(downKeyring{}))
	if err := s.Set(ctx, "OPENAI_API_KEY", secret.New("sk-env")); err != nil {
		t.Fatal(err)
	}
	got, err := New(dir, WithKeyring(downKeyring{})).Lookup(ctx, "OPENAI_API_KEY")
	if err != nil || got.Expose() != "sk-env" {
		t.Fatalf("lookup with the env passphrase: got %q err %v", got.Expose(), err)
	}
}

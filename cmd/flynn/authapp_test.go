package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/internal/vault"
	"github.com/ionalpha/flynn/secret"
	"github.com/ionalpha/flynn/tools/github"
)

// appTestVault returns a sealed-file vault in a temporary directory, so a test never
// touches the developer's keychain.
func appTestVault(t *testing.T) *vault.Store {
	t.Helper()
	t.Setenv("FLYNN_VAULT_FILE", "1")
	return vault.New(t.TempDir(), vault.WithPassphrase(func(bool) (secret.Text, error) {
		return secret.New("test-passphrase"), nil
	}))
}

// appKeyFile writes a freshly generated RSA key in PEM form and returns its path. The
// armor is produced at runtime, so no key-like literal appears in the source tree.
func appKeyFile(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "app.pem")
	body := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestAuthSetAppStoresTheIdentityInTheVault is the point of the command: after it runs,
// a review authenticates from the vault with no environment variable set anywhere.
func TestAuthSetAppStoresTheIdentityInTheVault(t *testing.T) {
	ctx := context.Background()
	store := appTestVault(t)
	keyPath := appKeyFile(t)

	err := authSetApp(ctx, store,
		[]string{"--issuer", "4259025", "--installation", "145518648", "--key-file", keyPath},
		os.ReadFile, os.Remove)
	if err != nil {
		t.Fatalf("set-app: %v", err)
	}

	for ref, want := range map[string]string{
		refAppIssuer:       "4259025",
		refAppInstallation: "145518648",
	} {
		got, err := store.Lookup(ctx, ref)
		if err != nil {
			t.Fatalf("%s not stored: %v", ref, err)
		}
		if got.Expose() != want {
			t.Errorf("%s = %q, want %q", ref, got.Expose(), want)
		}
	}
	key, err := store.Lookup(ctx, refAppKey)
	if err != nil {
		t.Fatalf("%s not stored: %v", refAppKey, err)
	}
	if _, err := github.ParsePrivateKey([]byte(key.Expose())); err != nil {
		t.Errorf("the stored key does not parse back: %v", err)
	}

	// The key file survives unless the operator asks for it to be forgotten.
	if _, err := os.Stat(keyPath); err != nil {
		t.Errorf("set-app deleted the key file without --forget-key-file: %v", err)
	}
}

// TestAuthSetAppForgetsTheKeyFile: the flag exists so the key stops living in
// plaintext on disk once the vault holds it.
func TestAuthSetAppForgetsTheKeyFile(t *testing.T) {
	ctx := context.Background()
	store := appTestVault(t)
	keyPath := appKeyFile(t)

	err := authSetApp(ctx, store,
		[]string{"--issuer", "1", "--installation", "2", "--key-file", keyPath, "--forget-key-file"},
		os.ReadFile, os.Remove)
	if err != nil {
		t.Fatalf("set-app: %v", err)
	}
	if _, err := os.Stat(keyPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the key file still exists after --forget-key-file (stat err = %v)", err)
	}
	if _, err := store.Lookup(ctx, refAppKey); err != nil {
		t.Fatalf("the key was deleted from disk but is not in the vault: %v", err)
	}
}

// TestAuthSetAppRefusesBadInput: every rejection happens before anything is written, so
// a failed set-app cannot leave a half-configured App that a review would refuse later
// with a confusing error.
func TestAuthSetAppRefusesBadInput(t *testing.T) {
	good := appKeyFile(t)
	notAKey := filepath.Join(t.TempDir(), "notakey.pem")
	if err := os.WriteFile(notAKey, []byte("this is not a pem\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cases := map[string][]string{
		"no issuer":                 {"--installation", "2", "--key-file", good},
		"no installation":           {"--issuer", "1", "--key-file", good},
		"no key file":               {"--issuer", "1", "--installation", "2"},
		"issuer not a number":       {"--issuer", "vouchbot", "--installation", "2", "--key-file", good},
		"installation not a number": {"--issuer", "1", "--installation", "the-first-one", "--key-file", good},
		"key file missing":          {"--issuer", "1", "--installation", "2", "--key-file", filepath.Join(t.TempDir(), "absent.pem")},
		"key file is not a key":     {"--issuer", "1", "--installation", "2", "--key-file", notAKey},
		"issuer zero":               {"--issuer", "0", "--installation", "2", "--key-file", good},
		"issuer negative":           {"--issuer", "-1", "--installation", "2", "--key-file", good},
		"installation zero":         {"--issuer", "1", "--installation", "0", "--key-file", good},
		"installation negative":     {"--issuer", "1", "--installation", "-7", "--key-file", good},
		"stray positional":          {"--issuer", "1", "--installation", "2", "--key-file", good, "extra.pem"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store := appTestVault(t)
			if err := authSetApp(ctx, store, args, os.ReadFile, os.Remove); err == nil {
				t.Fatal("expected an error")
			}
			for _, ref := range []string{refAppIssuer, refAppInstallation, refAppKey} {
				if _, err := store.Lookup(ctx, ref); err == nil {
					t.Errorf("a rejected set-app stored %s", ref)
				}
			}
		})
	}
}

// faultyStore is a vault whose Set fails on the reference it is told to fail on, so a
// write that succeeds part-way can be exercised.
type faultyStore struct {
	failOn  string
	stored  map[string]string
	deletes []string
}

func (f *faultyStore) Lookup(_ context.Context, ref string) (secret.Text, error) {
	if v, ok := f.stored[ref]; ok {
		return secret.New(v), nil
	}
	return secret.Text{}, secret.ErrNotFound
}

func (f *faultyStore) Set(_ context.Context, ref string, value secret.Text) error {
	if ref == f.failOn {
		return errors.New("vault is unavailable")
	}
	if f.stored == nil {
		f.stored = map[string]string{}
	}
	f.stored[ref] = value.Expose()
	return nil
}

func (f *faultyStore) Delete(_ context.Context, ref string) error {
	f.deletes = append(f.deletes, ref)
	delete(f.stored, ref)
	return nil
}

// TestAuthSetAppRollsBackAPartialWrite: a vault that fails on the second reference
// must leave nothing behind. A stored issuer with no key is worse than no App at all,
// because a review refuses a part-configured App instead of falling back to a token,
// so a failed set-app would break the next review rather than leave it as it was.
func TestAuthSetAppRollsBackAPartialWrite(t *testing.T) {
	keyPath := appKeyFile(t)
	for _, failOn := range []string{refAppInstallation, refAppKey} {
		t.Run(failOn, func(t *testing.T) {
			store := &faultyStore{failOn: failOn}
			err := authSetApp(context.Background(), store,
				[]string{"--issuer", "1", "--installation", "2", "--key-file", keyPath},
				os.ReadFile, os.Remove)
			if err == nil {
				t.Fatal("a failing vault write must fail the command")
			}
			if len(store.stored) != 0 {
				t.Errorf("set-app left %v in the vault after failing on %s", store.stored, failOn)
			}
			if len(store.deletes) == 0 {
				t.Errorf("set-app did not roll back what it had written before failing on %s", failOn)
			}
		})
	}
}

// TestAuthSetAppRestoresTheReplacedIdentityOnFailure is the rotation case. set-app over
// an existing App is how a key is rotated, so a write that fails part-way is replacing
// something that worked. Rolling back by deleting would destroy the working identity
// and leave the operator with no App at all, which is strictly worse than the failed
// rotation they asked for.
func TestAuthSetAppRestoresTheReplacedIdentityOnFailure(t *testing.T) {
	keyPath := appKeyFile(t)
	store := &faultyStore{failOn: refAppKey, stored: map[string]string{
		refAppIssuer:       "1111",
		refAppInstallation: "2222",
		refAppKey:          "the-old-key",
	}}

	err := authSetApp(context.Background(), store,
		[]string{"--issuer", "9999", "--installation", "8888", "--key-file", keyPath},
		os.ReadFile, os.Remove)
	if err == nil {
		t.Fatal("a failing vault write must fail the command")
	}
	want := map[string]string{
		refAppIssuer:       "1111",
		refAppInstallation: "2222",
		refAppKey:          "the-old-key",
	}
	for ref, value := range want {
		got, ok := store.stored[ref]
		if !ok {
			t.Errorf("a failed rotation deleted %s, destroying the App that still worked", ref)
			continue
		}
		if got != value {
			t.Errorf("%s = %q after a failed rotation, want the previous %q", ref, got, value)
		}
	}
}

// TestAuthRemoveAppClearsEveryReference is the rotation path, and it must clear a
// partial identity too: a key removed but an issuer left behind would make the next
// review fail on a half-configured App rather than fall through to a token.
func TestAuthRemoveAppClearsEveryReference(t *testing.T) {
	ctx := context.Background()
	store := appTestVault(t)

	if err := store.Set(ctx, refAppIssuer, secret.New("4259025")); err != nil {
		t.Fatal(err)
	}
	if err := authRemoveApp(ctx, store); err != nil {
		t.Fatalf("rm-app with a partial identity: %v", err)
	}
	for _, ref := range []string{refAppIssuer, refAppInstallation, refAppKey} {
		if _, err := store.Lookup(ctx, ref); err == nil {
			t.Errorf("%s survived rm-app", ref)
		}
	}
}

// TestAppIdentityStatus: a review needs all three references and refuses a partial set,
// so the listing must not report an App as usable when the next review will reject it.
func TestAppIdentityStatus(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name  string
		store map[string]string
		want  string
	}{
		{"none", nil, "not set"},
		{"key only", map[string]string{refAppKey: "k"}, "partial"},
		{"missing key", map[string]string{refAppIssuer: "1", refAppInstallation: "2"}, "partial"},
		{"complete", map[string]string{refAppIssuer: "1", refAppInstallation: "2", refAppKey: "k"}, "stored"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := appIdentityStatus(ctx, &faultyStore{stored: tc.store})
			if !strings.HasPrefix(got, tc.want) {
				t.Errorf("appIdentityStatus = %q, want it to start with %q", got, tc.want)
			}
		})
	}
}

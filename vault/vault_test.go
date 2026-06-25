package vault

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/secret"
)

// fakeKeyring is an in-memory Keyring for the keychain-available path.
type fakeKeyring map[string]string

func (f fakeKeyring) Get(_, key string) (string, error) {
	if v, ok := f[key]; ok {
		return v, nil
	}
	return "", ErrKeyNotFound
}
func (f fakeKeyring) Set(_, key, val string) error { f[key] = val; return nil }
func (f fakeKeyring) Delete(_, key string) error   { delete(f, key); return nil }

// downKeyring stands in for a host with no keychain: every operation fails, so the
// store must fall back to the sealed file.
type downKeyring struct{}

var errNoKeyring = errors.New("no keyring service")

func (downKeyring) Get(_, _ string) (string, error) { return "", errNoKeyring }
func (downKeyring) Set(_, _, _ string) error        { return errNoKeyring }
func (downKeyring) Delete(_, _ string) error        { return errNoKeyring }

func fixedPass(p string) Passphrase {
	return func(bool) (secret.Text, error) { return secret.New(p), nil }
}

func TestSealRoundTrip(t *testing.T) {
	blob, err := seal([]byte("super-secret-value"), []byte("correct horse"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := open(blob, []byte("correct horse"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "super-secret-value" {
		t.Fatalf("round-trip got %q", got)
	}
}

func TestOpenWrongPassphrase(t *testing.T) {
	blob, _ := seal([]byte("x"), []byte("right"))
	if _, err := open(blob, []byte("wrong")); !errors.Is(err, ErrBadPassphrase) {
		t.Fatalf("wrong passphrase: got %v, want ErrBadPassphrase", err)
	}
}

func TestOpenTamperedBlobFails(t *testing.T) {
	blob, _ := seal([]byte("x"), []byte("pass"))
	blob[len(blob)-3] ^= 0xff // flip a ciphertext byte
	if _, err := open(blob, []byte("pass")); err == nil {
		t.Fatal("tampered blob should not open")
	}
}

func TestSealIsNondeterministic(t *testing.T) {
	a, _ := seal([]byte("same"), []byte("pass"))
	b, _ := seal([]byte("same"), []byte("pass"))
	if string(a) == string(b) {
		t.Fatal("two seals of the same value were identical (salt/nonce not random)")
	}
}

func TestStoreKeychainPath(t *testing.T) {
	kr := fakeKeyring{}
	s := New(t.TempDir(), WithKeyring(kr))
	ctx := context.Background()

	if err := s.Set(ctx, "OPENAI_API_KEY", secret.New("sk-123")); err != nil {
		t.Fatal(err)
	}
	got, err := s.Lookup(ctx, "OPENAI_API_KEY")
	if err != nil || got.Expose() != "sk-123" {
		t.Fatalf("lookup got %q err %v", got.Expose(), err)
	}
	if _, err := s.Lookup(ctx, "ABSENT"); !errors.Is(err, secret.ErrNotFound) {
		t.Fatalf("absent key: got %v, want ErrNotFound", err)
	}
	if err := s.Delete(ctx, "OPENAI_API_KEY"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Lookup(ctx, "OPENAI_API_KEY"); !errors.Is(err, secret.ErrNotFound) {
		t.Fatalf("after delete: got %v, want ErrNotFound", err)
	}
}

// TestStoreRoundTripProperty pins the store contract over arbitrary credentials:
// whatever is set can be looked up unchanged, and whatever is deleted is gone. It
// runs over the keychain backend so each iteration is cheap (the sealed-file
// crypto is covered exhaustively by the seal tests above).
func TestStoreRoundTripProperty(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		s := New(t.TempDir(), WithKeyring(fakeKeyring{}))
		ctx := context.Background()
		refGen := rapid.StringMatching(`[A-Z][A-Z0-9_]{0,12}`)

		creds := rapid.MapOfN(refGen, rapid.StringMatching(`[ -~]{1,40}`), 1, 8).Draw(rt, "creds")
		for ref, val := range creds {
			if err := s.Set(ctx, ref, secret.New(val)); err != nil {
				rt.Fatalf("set %q: %v", ref, err)
			}
		}
		for ref, val := range creds {
			got, err := s.Lookup(ctx, ref)
			if err != nil || got.Expose() != val {
				rt.Fatalf("lookup %q: got %q err %v, want %q", ref, got.Expose(), err, val)
			}
		}
		for ref := range creds {
			if err := s.Delete(ctx, ref); err != nil {
				rt.Fatalf("delete %q: %v", ref, err)
			}
			if _, err := s.Lookup(ctx, ref); !errors.Is(err, secret.ErrNotFound) {
				rt.Fatalf("after delete %q: got %v, want ErrNotFound", ref, err)
			}
		}
	})
}

func TestStoreSealedFileFallback(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	s := New(dir, WithKeyring(downKeyring{}), WithPassphrase(fixedPass("unlock-me")))

	if err := s.Set(ctx, "OPENAI_API_KEY", secret.New("sk-file")); err != nil {
		t.Fatal(err)
	}

	// A fresh store over the same directory (simulating a later run) must read the
	// sealed value back with the same passphrase.
	s2 := New(dir, WithKeyring(downKeyring{}), WithPassphrase(fixedPass("unlock-me")))
	got, err := s2.Lookup(ctx, "OPENAI_API_KEY")
	if err != nil || got.Expose() != "sk-file" {
		t.Fatalf("reopen got %q err %v", got.Expose(), err)
	}

	// The wrong passphrase must fail to unlock.
	s3 := New(dir, WithKeyring(downKeyring{}), WithPassphrase(fixedPass("wrong")))
	if _, err := s3.Lookup(ctx, "OPENAI_API_KEY"); !errors.Is(err, ErrBadPassphrase) {
		t.Fatalf("wrong passphrase reopen: got %v, want ErrBadPassphrase", err)
	}
}

func TestStoreSealedFileMissingPassphrase(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	// No passphrase supplier and no keychain: a write cannot seal.
	s := New(dir, WithKeyring(downKeyring{}), WithPassphrase(func(bool) (secret.Text, error) {
		return secret.Text{}, ErrNoPassphrase
	}))
	if err := s.Set(ctx, "K", secret.New("v")); !errors.Is(err, ErrNoPassphrase) {
		t.Fatalf("set without passphrase: got %v, want ErrNoPassphrase", err)
	}
}

func TestStoreSealedFileIsEncryptedAtRest(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	s := New(dir, WithKeyring(downKeyring{}), WithPassphrase(fixedPass("pw")))
	if err := s.Set(ctx, "OPENAI_API_KEY", secret.New("sk-plaintext-must-not-appear")); err != nil {
		t.Fatal(err)
	}
	// The on-disk file must not contain the plaintext value anywhere.
	if !s.fileExists() {
		t.Fatal("expected a sealed file to exist")
	}
	blob, err := os.ReadFile(s.file)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), "sk-plaintext-must-not-appear") {
		t.Fatalf("sealed file leaked the plaintext:\n%s", blob)
	}
}

package vault

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// blockingKeyring simulates a locked or headless OS keychain: every operation blocks
// until released, standing in for go-keyring's /usr/bin/security call hanging on a
// keychain that never unlocks. The release channel lets a test unblock the goroutines
// so they exit rather than leak.
type blockingKeyring struct{ release <-chan struct{} }

func (b blockingKeyring) Get(_, _ string) (string, error) { <-b.release; return "", ErrKeyNotFound }
func (b blockingKeyring) Set(_, _, _ string) error        { <-b.release; return nil }
func (b blockingKeyring) Delete(_, _ string) error        { <-b.release; return nil }

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

func TestOpenRejectsHostileKDFParams(t *testing.T) {
	// Seal a valid blob, then mutate only the self-describing KDF parameters to
	// hostile values. open must reject each as ErrBadPassphrase before deriving the
	// key, never attempt a giant allocation or panic on a zero parameter.
	base, _ := seal([]byte("x"), []byte("pass"))
	var f sealFormat
	if err := json.Unmarshal(base, &f); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*sealFormat){
		"giant memory": func(s *sealFormat) { s.MemoryKiB = 1 << 30 }, // ~1 TiB
		"zero memory":  func(s *sealFormat) { s.MemoryKiB = 0 },
		"zero time":    func(s *sealFormat) { s.Time = 0 },
		"zero threads": func(s *sealFormat) { s.Threads = 0 },
		"empty salt":   func(s *sealFormat) { s.Salt = nil },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			bad := f
			mutate(&bad)
			blob, err := json.Marshal(bad)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := open(blob, []byte("pass")); !errors.Is(err, ErrBadPassphrase) {
				t.Fatalf("hostile params: got %v, want ErrBadPassphrase", err)
			}
		})
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

func TestTimeoutKeyringBoundsAHang(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) }) // let the blocked goroutines exit
	stuck := timeoutKeyring{inner: blockingKeyring{release: release}, timeout: 20 * time.Millisecond}

	if _, err := stuck.Get("svc", "k"); !errors.Is(err, errKeychainTimeout) {
		t.Fatalf("Get on a stuck keychain: got %v, want errKeychainTimeout", err)
	}
	if err := stuck.Set("svc", "k", "v"); !errors.Is(err, errKeychainTimeout) {
		t.Fatalf("Set on a stuck keychain: got %v, want errKeychainTimeout", err)
	}
	if err := stuck.Delete("svc", "k"); !errors.Is(err, errKeychainTimeout) {
		t.Fatalf("Delete on a stuck keychain: got %v, want errKeychainTimeout", err)
	}

	// A responsive inner keychain passes its result straight through, so a healthy
	// desktop keychain is never cut off by the bound.
	fast := timeoutKeyring{inner: fakeKeyring{"k": "v"}, timeout: time.Second}
	if got, err := fast.Get("svc", "k"); err != nil || got != "v" {
		t.Fatalf("Get on a healthy keychain: got %q err %v, want \"v\"", got, err)
	}
}

func TestStoreFallsBackWhenKeychainHangs(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	stuck := timeoutKeyring{inner: blockingKeyring{release: release}, timeout: 20 * time.Millisecond}
	s := New(dir, WithKeyring(stuck), WithPassphrase(fixedPass("unlock-me")))

	// A hanging keychain must not hang the Store: the write seals into the file and the
	// read comes back from it, so a command completes instead of blocking forever.
	if err := s.Set(ctx, "OPENAI_API_KEY", secret.New("sk-file")); err != nil {
		t.Fatal(err)
	}
	got, err := s.Lookup(ctx, "OPENAI_API_KEY")
	if err != nil || got.Expose() != "sk-file" {
		t.Fatalf("lookup after keychain-timeout fallback: got %q err %v", got.Expose(), err)
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

// --- FLYNN_VAULT_FILE switch -------------------------------------------------

func TestForceFileVaultParsesEnv(t *testing.T) {
	for _, v := range []string{"1", "true", "on", "yes", "TRUE", " yes "} {
		t.Setenv("FLYNN_VAULT_FILE", v)
		if !forceFileVault() {
			t.Fatalf("FLYNN_VAULT_FILE=%q should force the file vault", v)
		}
	}
	for _, v := range []string{"", "0", "false", "no", "off", "bogus"} {
		t.Setenv("FLYNN_VAULT_FILE", v)
		if forceFileVault() {
			t.Fatalf("FLYNN_VAULT_FILE=%q must not force the file vault", v)
		}
	}
}

func TestDefaultKeyringHonorsSwitch(t *testing.T) {
	t.Setenv("FLYNN_VAULT_FILE", "")
	tk, ok := defaultKeyring().(timeoutKeyring)
	if !ok {
		t.Fatalf("default backend should be the timeout-bounded OS keychain, got %T", defaultKeyring())
	}
	if _, ok := tk.inner.(osKeyring); !ok {
		t.Fatalf("default backend should wrap the OS keychain, got inner %T", tk.inner)
	}
	t.Setenv("FLYNN_VAULT_FILE", "1")
	if _, ok := defaultKeyring().(disabledKeyring); !ok {
		t.Fatalf("FLYNN_VAULT_FILE should disable the keychain, got %T", defaultKeyring())
	}
}

// TestForceFileVaultUsesSealedFile is the end-to-end switch behavior: with
// FLYNN_VAULT_FILE set and no explicit keyring, a Set writes the passphrase-sealed file
// and a fresh Store reads the value back, so the OS keychain is bypassed entirely.
func TestForceFileVaultUsesSealedFile(t *testing.T) {
	t.Setenv("FLYNN_VAULT_FILE", "1")
	dir := t.TempDir()
	ctx := context.Background()

	s := New(dir, WithPassphrase(fixedPass("unlock-me")))
	if err := s.Set(ctx, "OPENAI_API_KEY", secret.New("sk-secret")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "vault.sealed")); err != nil {
		t.Fatalf("expected the sealed file to be written: %v", err)
	}

	got, err := New(dir, WithPassphrase(fixedPass("unlock-me"))).Lookup(ctx, "OPENAI_API_KEY")
	if err != nil {
		t.Fatal(err)
	}
	if got.Expose() != "sk-secret" {
		t.Fatalf("Lookup got %q, want %q", got.Expose(), "sk-secret")
	}

	// The sealed bytes on disk do not contain the plaintext key.
	blob, err := os.ReadFile(filepath.Join(dir, "vault.sealed"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), "sk-secret") {
		t.Fatal("plaintext key found in the sealed file")
	}
}

// TestWithKeyringOverridesSwitch confirms an explicit WithKeyring still wins, so the
// switch changes only the default and does not break a caller (or a test) that injects
// its own backend.
func TestWithKeyringOverridesSwitch(t *testing.T) {
	t.Setenv("FLYNN_VAULT_FILE", "1")
	kr := fakeKeyring{}
	ctx := context.Background()

	s := New(t.TempDir(), WithKeyring(kr))
	if err := s.Set(ctx, "K", secret.New("v")); err != nil {
		t.Fatal(err)
	}
	if kr["K"] != "v" {
		t.Fatal("WithKeyring should override FLYNN_VAULT_FILE and use the injected keyring")
	}
}

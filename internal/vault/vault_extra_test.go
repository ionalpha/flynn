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

	"github.com/zalando/go-keyring"
	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"

	"github.com/ionalpha/flynn/secret"
)

// resealWith encrypts plaintext under a key derived with f's own KDF parameters,
// producing the ciphertext a vault sealed with those parameters would carry. It
// lets a test build a well-formed blob whose parameters differ from today's
// defaults without reaching into seal, which always writes the defaults.
func resealWith(t *testing.T, f sealFormat, plaintext, passphrase []byte) []byte {
	t.Helper()
	key := argon2.IDKey(passphrase, f.Salt, f.Time, f.MemoryKiB, f.Threads, argonKeyLen)
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		t.Fatal(err)
	}
	return aead.Seal(nil, f.Nonce, plaintext, nil)
}

// recordingKeyring notes the service name every operation is issued under, so a
// test can prove the Store passes the configured service through to the backend.
type recordingKeyring struct {
	services []string
	values   map[string]string
}

func newRecordingKeyring() *recordingKeyring {
	return &recordingKeyring{values: map[string]string{}}
}

func (r *recordingKeyring) Get(service, key string) (string, error) {
	r.services = append(r.services, service)
	if v, ok := r.values[key]; ok {
		return v, nil
	}
	return "", ErrKeyNotFound
}

func (r *recordingKeyring) Set(service, key, val string) error {
	r.services = append(r.services, service)
	r.values[key] = val
	return nil
}

func (r *recordingKeyring) Delete(service, key string) error {
	r.services = append(r.services, service)
	delete(r.values, key)
	return nil
}

// --- service name ------------------------------------------------------------

func TestWithServiceNamesTheKeychainEntry(t *testing.T) {
	ctx := context.Background()
	kr := newRecordingKeyring()
	s := New(t.TempDir(), WithKeyring(kr), WithService("flynn-test"))

	if err := s.Set(ctx, "K", secret.New("v")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Lookup(ctx, "K"); err != nil {
		t.Fatal(err)
	}
	for _, got := range kr.services {
		if got != "flynn-test" {
			t.Fatalf("keychain operation used service %q, want %q", got, "flynn-test")
		}
	}
	if len(kr.services) != 2 {
		t.Fatalf("expected the Set and the Lookup to reach the keyring, saw %d ops", len(kr.services))
	}

	// An empty name is ignored so a caller cannot accidentally blank the service and
	// store credentials under "".
	def := New(t.TempDir(), WithKeyring(newRecordingKeyring()), WithService(""))
	if def.service != DefaultService {
		t.Fatalf("WithService(\"\") set service to %q, want the default %q", def.service, DefaultService)
	}
}

// --- EnvPassphrase -----------------------------------------------------------

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

// --- keyring backends --------------------------------------------------------

// TestOSKeyringTranslatesNotFound covers the real platform adapter against
// go-keyring's in-memory mock provider: the backend's own not-found error must be
// translated to ErrKeyNotFound, because that is the single signal the Store reads as
// "the keychain works and simply has no such entry" rather than "the keychain is
// unavailable, fall back to the sealed file". Getting that wrong would silently
// route every desktop lookup through the file.
func TestOSKeyringTranslatesNotFound(t *testing.T) {
	keyring.MockInit()
	var kr osKeyring

	if _, err := kr.Get("svc", "absent"); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("Get of an absent key: got %v, want ErrKeyNotFound", err)
	}
	if err := kr.Set("svc", "K", "v"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := kr.Get("svc", "K")
	if err != nil || got != "v" {
		t.Fatalf("Get after Set: got %q err %v, want %q", got, err, "v")
	}
	if err := kr.Delete("svc", "K"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := kr.Get("svc", "K"); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("Get after Delete: got %v, want ErrKeyNotFound", err)
	}
	// Deleting an absent key is not an error, so removing a credential twice (or
	// removing one that only ever lived in the sealed file) is safe.
	if err := kr.Delete("svc", "never-existed"); err != nil {
		t.Fatalf("Delete of an absent key: got %v, want nil", err)
	}
}

// TestOSKeyringSurfacesBackendFailure pins the other side: an error that is not
// go-keyring's not-found must pass through unchanged, so the Store sees "keychain
// unavailable" and falls back rather than reporting the credential as absent.
func TestOSKeyringSurfacesBackendFailure(t *testing.T) {
	boom := errors.New("no secret service")
	keyring.MockInitWithError(boom)
	t.Cleanup(keyring.MockInit) // leave the package back on the plain mock store
	var kr osKeyring

	if _, err := kr.Get("svc", "K"); !errors.Is(err, boom) {
		t.Fatalf("Get: got %v, want the backend error", err)
	}
	if err := kr.Set("svc", "K", "v"); !errors.Is(err, boom) {
		t.Fatalf("Set: got %v, want the backend error", err)
	}
	if err := kr.Delete("svc", "K"); !errors.Is(err, boom) {
		t.Fatalf("Delete: got %v, want the backend error", err)
	}
}

func TestDisabledKeyringIsAlwaysUnavailable(t *testing.T) {
	var kr disabledKeyring

	if _, err := kr.Get("svc", "K"); !errors.Is(err, errKeychainDisabled) {
		t.Fatalf("Get: got %v, want errKeychainDisabled", err)
	}
	if err := kr.Set("svc", "K", "v"); !errors.Is(err, errKeychainDisabled) {
		t.Fatalf("Set: got %v, want errKeychainDisabled", err)
	}
	if err := kr.Delete("svc", "K"); !errors.Is(err, errKeychainDisabled) {
		t.Fatalf("Delete: got %v, want errKeychainDisabled", err)
	}
	// The disabled backend must never report ErrKeyNotFound: that would make the
	// Store believe the keychain answered authoritatively and skip the sealed file.
	if _, err := kr.Get("svc", "K"); errors.Is(err, ErrKeyNotFound) {
		t.Fatal("the disabled keychain must not masquerade as an empty keychain")
	}
}

// TestTimeoutKeyringPassesInnerErrorsThrough confirms the bound only adds a
// deadline: an inner failure is reported verbatim, not converted to a timeout.
func TestTimeoutKeyringPassesInnerErrorsThrough(t *testing.T) {
	tk := timeoutKeyring{inner: downKeyring{}, timeout: time.Minute}

	if _, err := tk.Get("svc", "K"); !errors.Is(err, errNoKeyring) {
		t.Fatalf("Get: got %v, want the inner error", err)
	}
	if err := tk.Set("svc", "K", "v"); !errors.Is(err, errNoKeyring) {
		t.Fatalf("Set: got %v, want the inner error", err)
	}
	if err := tk.Delete("svc", "K"); !errors.Is(err, errNoKeyring) {
		t.Fatalf("Delete: got %v, want the inner error", err)
	}
	if _, err := tk.Get("svc", "K"); errors.Is(err, errKeychainTimeout) {
		t.Fatal("a fast inner failure must not be reported as a timeout")
	}
}

// --- sealed-file Delete ------------------------------------------------------

func TestStoreDeleteSealedFile(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := New(dir, WithKeyring(downKeyring{}), WithPassphrase(fixedPass("pw")))

	// Deleting before any vault file exists is a no-op, not an error, and must not
	// create a file (which would otherwise demand a passphrase for a vault that
	// holds nothing).
	if err := s.Delete(ctx, "ABSENT"); err != nil {
		t.Fatalf("delete with no vault file: %v", err)
	}
	if s.fileExists() {
		t.Fatal("deleting from a nonexistent vault created one")
	}

	if err := s.Set(ctx, "A", secret.New("va")); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(ctx, "B", secret.New("vb")); err != nil {
		t.Fatal(err)
	}

	// Deleting a key the vault does not hold leaves the file untouched.
	before, err := os.ReadFile(s.file)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "NOPE"); err != nil {
		t.Fatalf("delete of an absent key: %v", err)
	}
	after, err := os.ReadFile(s.file)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("deleting an absent key rewrote the sealed file")
	}

	// Deleting a held key removes it and leaves its neighbours intact.
	if err := s.Delete(ctx, "A"); err != nil {
		t.Fatalf("delete of a held key: %v", err)
	}
	if _, err := s.Lookup(ctx, "A"); !errors.Is(err, secret.ErrNotFound) {
		t.Fatalf("after delete: got %v, want ErrNotFound", err)
	}
	got, err := s.Lookup(ctx, "B")
	if err != nil || got.Expose() != "vb" {
		t.Fatalf("neighbour after delete: got %q err %v, want %q", got.Expose(), err, "vb")
	}
}

// TestStoreDeleteClearsBothBackends is the anti-resurrection rule: a credential
// removed from the keychain must not survive in a stale sealed-file copy and come
// back on the next lookup from a host where the keychain is unavailable.
func TestStoreDeleteClearsBothBackends(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	pass := WithPassphrase(fixedPass("pw"))

	// Seed the sealed file (keychain down), then delete with the keychain up.
	if err := New(dir, WithKeyring(downKeyring{}), pass).Set(ctx, "K", secret.New("stale")); err != nil {
		t.Fatal(err)
	}
	kr := fakeKeyring{"K": "fresh"}
	if err := New(dir, WithKeyring(kr), pass).Delete(ctx, "K"); err != nil {
		t.Fatal(err)
	}
	if _, ok := kr["K"]; ok {
		t.Fatal("Delete left the credential in the keychain")
	}

	// The sealed-file copy must be gone too, so a later run on a keychain-less host
	// does not resurrect it.
	if _, err := New(dir, WithKeyring(downKeyring{}), pass).Lookup(ctx, "K"); !errors.Is(err, secret.ErrNotFound) {
		t.Fatalf("the sealed file resurrected a deleted credential: got %v, want ErrNotFound", err)
	}
}

// --- sealed-file failure paths -----------------------------------------------

// TestSealedFileErrorsPropagate pins that an unreadable or unopenable vault is
// reported on every path that touches it, never quietly treated as an empty vault.
// Swallowing the error would let a Set overwrite a vault it could not read, dropping
// every credential already in it.
func TestSealedFileErrorsPropagate(t *testing.T) {
	ctx := context.Background()

	ops := map[string]func(*Store) error{
		"Lookup": func(s *Store) error { _, err := s.Lookup(ctx, "K"); return err },
		"Set":    func(s *Store) error { return s.Set(ctx, "K", secret.New("v")) },
		"Delete": func(s *Store) error { return s.Delete(ctx, "K") },
	}

	t.Run("wrong passphrase", func(t *testing.T) {
		dir := t.TempDir()
		if err := New(dir, WithKeyring(downKeyring{}), WithPassphrase(fixedPass("right"))).
			Set(ctx, "K", secret.New("v")); err != nil {
			t.Fatal(err)
		}
		for name, op := range ops {
			t.Run(name, func(t *testing.T) {
				s := New(dir, WithKeyring(downKeyring{}), WithPassphrase(fixedPass("wrong")))
				if err := op(s); !errors.Is(err, ErrBadPassphrase) {
					t.Fatalf("got %v, want ErrBadPassphrase", err)
				}
			})
		}
	})

	t.Run("no passphrase supplier", func(t *testing.T) {
		dir := t.TempDir()
		// Seal a real vault so the file exists and must be opened.
		if err := New(dir, WithKeyring(downKeyring{}), WithPassphrase(fixedPass("pw"))).
			Set(ctx, "K", secret.New("v")); err != nil {
			t.Fatal(err)
		}
		for name, op := range ops {
			t.Run(name, func(t *testing.T) {
				s := New(dir, WithKeyring(downKeyring{}), WithPassphrase(nil))
				if err := op(s); !errors.Is(err, ErrNoPassphrase) {
					t.Fatalf("got %v, want ErrNoPassphrase", err)
				}
			})
		}
	})

	t.Run("empty passphrase", func(t *testing.T) {
		dir := t.TempDir()
		s := New(dir, WithKeyring(downKeyring{}), WithPassphrase(fixedPass("")))
		if err := s.Set(ctx, "K", secret.New("v")); !errors.Is(err, ErrNoPassphrase) {
			t.Fatalf("an empty passphrase must not seal a vault: got %v, want ErrNoPassphrase", err)
		}
		if s.fileExists() {
			t.Fatal("a rejected passphrase still wrote a vault file")
		}
	})

	t.Run("passphrase prompt fails", func(t *testing.T) {
		boom := errors.New("terminal closed")
		s := New(t.TempDir(), WithKeyring(downKeyring{}), WithPassphrase(func(bool) (secret.Text, error) {
			return secret.Text{}, boom
		}))
		if err := s.Set(ctx, "K", secret.New("v")); !errors.Is(err, boom) {
			t.Fatalf("got %v, want the prompt error", err)
		}
	})

	t.Run("unreadable vault path", func(t *testing.T) {
		// A directory where the vault file should be: os.ReadFile fails with
		// something other than ErrNotExist, which must surface rather than read as
		// "no vault yet".
		dir := t.TempDir()
		s := New(dir, WithKeyring(downKeyring{}), WithPassphrase(fixedPass("pw")))
		if err := os.MkdirAll(s.file, 0o700); err != nil {
			t.Fatal(err)
		}
		_, err := s.Lookup(ctx, "K")
		if err == nil {
			t.Fatal("a vault path that is a directory should not read as an empty vault")
		}
		if errors.Is(err, secret.ErrNotFound) {
			t.Fatalf("an unreadable vault was reported as an absent credential: %v", err)
		}
		if !strings.Contains(err.Error(), "read sealed file") {
			t.Fatalf("got %v, want a read-sealed-file error", err)
		}
	})
}

// TestNewVaultFlagIsSetOnlyForACreate proves the Passphrase callback learns whether
// it is creating a vault (so an interactive prompt can ask for confirmation) or
// opening an existing one (where a second prompt would be wrong).
func TestNewVaultFlagIsSetOnlyForACreate(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	var seen []bool
	pass := WithPassphrase(func(newVault bool) (secret.Text, error) {
		seen = append(seen, newVault)
		return secret.New("pw"), nil
	})

	s := New(dir, WithKeyring(downKeyring{}), pass)
	if err := s.Set(ctx, "A", secret.New("va")); err != nil { // creates the vault
		t.Fatal(err)
	}
	if err := s.Set(ctx, "B", secret.New("vb")); err != nil { // opens, then rewrites
		t.Fatal(err)
	}
	if len(seen) == 0 || !seen[0] {
		t.Fatalf("the first seal should report a new vault, saw %v", seen)
	}
	for _, newVault := range seen[1:] {
		if newVault {
			t.Fatalf("an existing vault was reported as new, saw %v", seen)
		}
	}
}

// TestLoadFileRejectsMalformedContents covers the last decode step: a blob that
// authenticates (right passphrase, untampered) but does not hold the credential map
// must be reported, not returned as a nil map that reads as an empty vault.
func TestLoadFileRejectsMalformedContents(t *testing.T) {
	dir := t.TempDir()
	s := New(dir, WithKeyring(downKeyring{}), WithPassphrase(fixedPass("pw")))

	blob, err := seal([]byte("this is not the credential map"), []byte("pw"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.file, blob, 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = s.Lookup(context.Background(), "K")
	if err == nil || !strings.Contains(err.Error(), "malformed vault contents") {
		t.Fatalf("got %v, want a malformed-contents error", err)
	}
}

// TestSetIntoANullVault covers the nil-map guard in Set. A sealed blob whose
// payload is the JSON literal "null" decodes to a nil map with no error, so without
// the guard the very next assignment would panic. It is the one way a valid,
// correctly-sealed vault can still hand back a nil map.
func TestSetIntoANullVault(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := New(dir, WithKeyring(downKeyring{}), WithPassphrase(fixedPass("pw")))

	blob, err := seal([]byte("null"), []byte("pw"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.file, blob, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := s.Set(ctx, "K", secret.New("v")); err != nil {
		t.Fatalf("setting into a null vault: %v", err)
	}
	got, err := s.Lookup(ctx, "K")
	if err != nil || got.Expose() != "v" {
		t.Fatalf("lookup after seeding a null vault: got %q err %v, want %q", got.Expose(), err, "v")
	}
}

// TestSaveFileCannotCreateItsDirectory pins that a vault whose parent directory
// cannot exist reports the failure instead of silently dropping the credential.
func TestSaveFileCannotCreateItsDirectory(t *testing.T) {
	root := t.TempDir()
	blocker := filepath.Join(root, "blocked")
	if err := os.WriteFile(blocker, []byte("i am a file, not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The vault would have to live inside a path component that is a regular file,
	// so MkdirAll cannot succeed.
	s := New(filepath.Join(blocker, "sub"), WithKeyring(downKeyring{}), WithPassphrase(fixedPass("pw")))

	if err := s.Set(context.Background(), "K", secret.New("v")); err == nil {
		t.Fatal("sealing into an uncreatable directory should fail")
	}
}

// --- seal / open edge cases --------------------------------------------------

func TestOpenRejectsMalformedBlobs(t *testing.T) {
	good, err := seal([]byte("payload"), []byte("pw"))
	if err != nil {
		t.Fatal(err)
	}
	var f sealFormat
	if err := json.Unmarshal(good, &f); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		blob    func(t *testing.T) []byte
		wantErr error  // when the failure has a sentinel
		wantMsg string // otherwise a distinguishing fragment
	}{
		{
			name:    "not JSON at all",
			blob:    func(*testing.T) []byte { return []byte("{{{ not json") },
			wantMsg: "malformed sealed blob",
		},
		{
			name: "unsupported version",
			blob: func(t *testing.T) []byte {
				bad := f
				bad.Version = sealVersion + 1
				b, err := json.Marshal(bad)
				if err != nil {
					t.Fatal(err)
				}
				return b
			},
			wantMsg: "unsupported seal version",
		},
		{
			name: "truncated nonce",
			blob: func(t *testing.T) []byte {
				bad := f
				bad.Nonce = bad.Nonce[:len(bad.Nonce)-1]
				b, err := json.Marshal(bad)
				if err != nil {
					t.Fatal(err)
				}
				return b
			},
			wantErr: ErrBadPassphrase,
		},
		{
			name: "empty ciphertext",
			blob: func(t *testing.T) []byte {
				bad := f
				bad.Ciphertext = nil
				b, err := json.Marshal(bad)
				if err != nil {
					t.Fatal(err)
				}
				return b
			},
			wantErr: ErrBadPassphrase,
		},
		{
			name: "salt swapped for another seal's",
			blob: func(t *testing.T) []byte {
				other, err := seal([]byte("payload"), []byte("pw"))
				if err != nil {
					t.Fatal(err)
				}
				var of sealFormat
				if err := json.Unmarshal(other, &of); err != nil {
					t.Fatal(err)
				}
				bad := f
				bad.Salt = of.Salt // derives a different key, so the AEAD must reject
				b, err := json.Marshal(bad)
				if err != nil {
					t.Fatal(err)
				}
				return b
			},
			wantErr: ErrBadPassphrase,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			plain, err := open(tc.blob(t), []byte("pw"))
			if plain != nil {
				t.Fatalf("a rejected blob still yielded plaintext: %q", plain)
			}
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("got %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantMsg) {
				t.Fatalf("got %v, want an error containing %q", err, tc.wantMsg)
			}
		})
	}
}

// TestOpenAcceptsRetunedKDFParams is the forward-compatibility half of the
// self-describing header: a vault sealed with parameters different from today's
// defaults still opens, as long as they are within the accepted bounds. Without
// this, tuning the defaults would lock users out of their existing vaults.
func TestOpenAcceptsRetunedKDFParams(t *testing.T) {
	// Seal by hand with in-bounds parameters that are not the current defaults.
	const (
		altTime    = uint32(1)
		altMemory  = uint32(8 * 1024)
		altThreads = uint8(1)
	)
	if altTime == argonTime && altMemory == argonMemoryKiB && altThreads == argonThreads {
		t.Fatal("the alternate parameters must differ from the defaults")
	}

	base, err := seal([]byte("payload"), []byte("pw"))
	if err != nil {
		t.Fatal(err)
	}
	var f sealFormat
	if err := json.Unmarshal(base, &f); err != nil {
		t.Fatal(err)
	}
	f.Time, f.MemoryKiB, f.Threads = altTime, altMemory, altThreads
	if !f.validKDFParams() {
		t.Fatal("the alternate parameters should be inside the accepted bounds")
	}
	// Re-seal the payload under a key derived with those parameters.
	f.Ciphertext = resealWith(t, f, []byte("payload"), []byte("pw"))
	blob, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}

	got, err := open(blob, []byte("pw"))
	if err != nil {
		t.Fatalf("a vault sealed with retuned parameters must still open: %v", err)
	}
	if string(got) != "payload" {
		t.Fatalf("open = %q, want %q", got, "payload")
	}
}

// TestValidKDFParamsBounds walks the accept/reject boundary of the parameter guard
// directly, including the lanes rule (Argon2id needs at least eight memory blocks
// per lane), which the whole-blob tests cannot reach individually.
func TestValidKDFParamsBounds(t *testing.T) {
	base := sealFormat{
		Version:   sealVersion,
		Time:      argonTime,
		MemoryKiB: argonMemoryKiB,
		Threads:   argonThreads,
		Salt:      make([]byte, saltLen),
	}
	tests := []struct {
		name   string
		mutate func(*sealFormat)
		want   bool
	}{
		{"defaults", func(*sealFormat) {}, true},
		{"time at the ceiling", func(f *sealFormat) { f.Time = maxArgonTime }, true},
		{"time above the ceiling", func(f *sealFormat) { f.Time = maxArgonTime + 1 }, false},
		{"threads at the ceiling", func(f *sealFormat) { f.Threads = maxArgonThreads }, true},
		{"threads above the ceiling", func(f *sealFormat) { f.Threads = maxArgonThreads + 1 }, false},
		{"memory at the ceiling", func(f *sealFormat) { f.MemoryKiB = maxArgonMemoryKiB }, true},
		{"memory above the ceiling", func(f *sealFormat) { f.MemoryKiB = maxArgonMemoryKiB + 1 }, false},
		{"memory below eight blocks per lane", func(f *sealFormat) {
			f.Threads = 4
			f.MemoryKiB = 8*4 - 1
		}, false},
		{"memory at exactly eight blocks per lane", func(f *sealFormat) {
			f.Threads = 4
			f.MemoryKiB = 8 * 4
		}, true},
		{"salt too short", func(f *sealFormat) { f.Salt = make([]byte, 7) }, false},
		{"salt at the floor", func(f *sealFormat) { f.Salt = make([]byte, 8) }, true},
		{"salt at the ceiling", func(f *sealFormat) { f.Salt = make([]byte, maxSaltLen) }, true},
		{"salt above the ceiling", func(f *sealFormat) { f.Salt = make([]byte, maxSaltLen+1) }, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := base
			f.Salt = append([]byte(nil), base.Salt...)
			tc.mutate(&f)
			if got := f.validKDFParams(); got != tc.want {
				t.Fatalf("validKDFParams() = %v, want %v", got, tc.want)
			}
		})
	}
}

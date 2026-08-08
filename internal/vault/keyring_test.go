package vault

// The OS keyring behind the vault, and the service name every operation is issued
// under. The backends are interchangeable only if they agree on what absence means, so
// a not-found is translated to one error while a backend that actually broke surfaces
// as itself. A disabled keyring is always unavailable rather than sometimes empty.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zalando/go-keyring"

	"github.com/ionalpha/flynn/secret"
)

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

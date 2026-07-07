package vault

import (
	"context"
	"errors"
	"time"

	"github.com/zalando/go-keyring"
)

// Keyring is the OS-backed secret store the vault prefers: the platform keychain
// that encrypts at rest under the user's login (Windows Credential Manager / DPAPI,
// macOS Keychain, Linux Secret Service). It is abstracted as an interface so the
// vault can run against a fake in tests and so a host with no keychain falls back
// to the passphrase-sealed file. A value stored here never touches the agent's
// disk in plaintext; the OS holds and guards it.
type Keyring interface {
	// Get returns the secret stored under key, or ErrKeyNotFound if none is.
	Get(service, key string) (string, error)
	// Set stores secret under key, replacing any existing value.
	Set(service, key, secret string) error
	// Delete removes the secret under key. Removing an absent key is not an error.
	Delete(service, key string) error
}

// ErrKeyNotFound reports that a key is absent from the keyring (distinct from the
// keyring being unavailable, which surfaces as a different error from the backend).
var ErrKeyNotFound = errors.New("vault: key not found in keyring")

// osKeyring is the real Keyring backed by the host platform keychain.
type osKeyring struct{}

// keychainOpTimeout bounds a single OS keychain operation. On a locked or headless
// macOS login keychain (an SSH session, a CI runner, a Mac with no GUI login),
// go-keyring's call into /usr/bin/security can block indefinitely waiting for an
// unlock the environment can never provide, which would hang the whole process
// before it prints anything. Bounding the call turns that hang into an error the
// Store treats as "keychain unavailable", so it falls back to the sealed file. The
// bound is far above a healthy keychain's millisecond response, so an unlocked
// desktop keychain is never cut off; only a genuinely stuck one hits it.
const keychainOpTimeout = 10 * time.Second

// errKeychainTimeout reports that an OS keychain operation did not return within
// keychainOpTimeout. Like errKeychainDisabled it is distinct from ErrKeyNotFound, so
// the Store treats it as "keychain unavailable" and falls through to the sealed file.
var errKeychainTimeout = errors.New("vault: OS keychain timed out (locked or headless; set FLYNN_VAULT_FILE=1 to skip it)")

// OSKeyring returns the platform keychain as a Keyring, with each operation bounded by
// keychainOpTimeout so a stuck keychain never hangs the process. On a host with no
// keychain service its operations fail, and the vault falls back to the sealed file.
func OSKeyring() Keyring { return timeoutKeyring{inner: osKeyring{}, timeout: keychainOpTimeout} }

// timeoutKeyring bounds each operation of an inner Keyring, returning errKeychainTimeout
// if the inner call does not complete in time. The inner call runs in its own goroutine
// with a buffered result channel, so when the stuck operation finally returns it sends
// and the goroutine exits rather than leaking; only the caller stops waiting. The bound
// comes from a context deadline, not a bare timer, so it never touches an external clock
// that has to replay deterministically.
type timeoutKeyring struct {
	inner   Keyring
	timeout time.Duration
}

func (t timeoutKeyring) Get(service, key string) (string, error) {
	type res struct {
		v   string
		err error
	}
	ch := make(chan res, 1)
	go func() {
		v, err := t.inner.Get(service, key)
		ch <- res{v, err}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), t.timeout)
	defer cancel()
	select {
	case r := <-ch:
		return r.v, r.err
	case <-ctx.Done():
		return "", errKeychainTimeout
	}
}

func (t timeoutKeyring) Set(service, key, secret string) error {
	ch := make(chan error, 1)
	go func() { ch <- t.inner.Set(service, key, secret) }()
	ctx, cancel := context.WithTimeout(context.Background(), t.timeout)
	defer cancel()
	select {
	case err := <-ch:
		return err
	case <-ctx.Done():
		return errKeychainTimeout
	}
}

func (t timeoutKeyring) Delete(service, key string) error {
	ch := make(chan error, 1)
	go func() { ch <- t.inner.Delete(service, key) }()
	ctx, cancel := context.WithTimeout(context.Background(), t.timeout)
	defer cancel()
	select {
	case err := <-ch:
		return err
	case <-ctx.Done():
		return errKeychainTimeout
	}
}

func (osKeyring) Get(service, key string) (string, error) {
	v, err := keyring.Get(service, key)
	if errors.Is(err, keyring.ErrNotFound) {
		return "", ErrKeyNotFound
	}
	return v, err
}

func (osKeyring) Set(service, key, secret string) error {
	return keyring.Set(service, key, secret)
}

func (osKeyring) Delete(service, key string) error {
	err := keyring.Delete(service, key)
	if errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	return err
}

// errKeychainDisabled reports that the OS keychain has been turned off (see
// disabledKeyring). It is distinct from ErrKeyNotFound, so the Store treats it as
// "keychain unavailable" and falls through to the passphrase-sealed file, exactly as a
// host with no keychain service does.
var errKeychainDisabled = errors.New("vault: OS keychain disabled")

// disabledKeyring is a Keyring whose every operation reports the keychain as unavailable,
// so the Store uses only the sealed file. It backs the FLYNN_VAULT_FILE switch: a run
// that opts into a file-only vault (a container, a CI job, a host whose keychain the user
// does not want touched) never reads or writes the OS keychain.
type disabledKeyring struct{}

func (disabledKeyring) Get(_, _ string) (string, error) { return "", errKeychainDisabled }
func (disabledKeyring) Set(_, _, _ string) error        { return errKeychainDisabled }
func (disabledKeyring) Delete(_, _ string) error        { return errKeychainDisabled }

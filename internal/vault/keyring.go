package vault

import (
	"context"
	"errors"
	"time"

	"github.com/zalando/go-keyring"
)

// Keyring is the OS secret store the vault prefers (Windows Credential Manager and
// DPAPI, macOS Keychain, Linux Secret Service), which encrypts at rest under the user's
// login so a value never reaches the agent's disk in plaintext. It is an interface so
// tests use a fake and a host with no keychain falls back to the passphrase-sealed
// file.
type Keyring interface {
	// Get returns the secret stored under key, or ErrKeyNotFound if none is.
	Get(service, key string) (string, error)
	// Set stores secret under key, replacing any existing value.
	Set(service, key, secret string) error
	// Delete removes the secret under key. Removing an absent key is not an error.
	Delete(service, key string) error
}

// ErrKeyNotFound reports that a key is absent from the keyring. An unavailable keyring
// surfaces as a different backend error.
var ErrKeyNotFound = errors.New("vault: key not found in keyring")

type osKeyring struct{}

// keychainOpTimeout bounds one OS keychain operation. On a locked or headless macOS
// login keychain (SSH, CI, no GUI login), go-keyring's call into /usr/bin/security can
// block forever waiting for an unlock, hanging the process before it prints anything.
// A timeout is an error the Store treats as keychain unavailable, so it falls back to
// the sealed file. A healthy keychain answers in milliseconds, far below the bound.
const keychainOpTimeout = 10 * time.Second

// errKeychainTimeout is distinct from ErrKeyNotFound, so the Store treats a timeout as
// keychain unavailable and falls through to the sealed file.
var errKeychainTimeout = errors.New("vault: OS keychain timed out (locked or headless; set FLYNN_VAULT_FILE=1 to skip it)")

// OSKeyring returns the platform keychain with each operation bounded by
// keychainOpTimeout. On a host with no keychain service its operations fail, and the
// vault falls back to the sealed file.
func OSKeyring() Keyring { return timeoutKeyring{inner: osKeyring{}, timeout: keychainOpTimeout} }

// timeoutKeyring bounds each operation of an inner Keyring, returning
// errKeychainTimeout when it overruns. The inner call runs in its own goroutine with a
// buffered result channel, so a stuck call that finally returns can send and exit
// instead of leaking. The bound is a context deadline, so it touches no clock that has
// to replay deterministically.
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

// errKeychainDisabled is distinct from ErrKeyNotFound, so the Store falls through to the
// sealed file as on a host with no keychain service.
var errKeychainDisabled = errors.New("vault: OS keychain disabled")

// disabledKeyring reports the keychain unavailable on every operation. It backs
// FLYNN_VAULT_FILE, so a file-only run (a container, a CI job, a user who wants the
// keychain untouched) never reads or writes the OS keychain.
type disabledKeyring struct{}

func (disabledKeyring) Get(_, _ string) (string, error) { return "", errKeychainDisabled }
func (disabledKeyring) Set(_, _, _ string) error        { return errKeychainDisabled }
func (disabledKeyring) Delete(_, _ string) error        { return errKeychainDisabled }

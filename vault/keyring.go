package vault

import (
	"errors"

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

// OSKeyring returns the platform keychain as a Keyring. On a host with no keychain
// service its operations fail, and the vault falls back to the sealed file.
func OSKeyring() Keyring { return osKeyring{} }

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

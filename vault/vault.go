// Package vault is Flynn's credential store: a secret.Source that holds API keys
// and other credentials encrypted at rest, so a user enters a key once and it is
// never written to disk in plaintext, never echoed, and revealed only at the point
// of use (the model request's authorization header).
//
// It has two backends behind one interface, tried in order. The OS keychain
// (Windows Credential Manager / DPAPI, macOS Keychain, Linux Secret Service) is
// preferred: the platform encrypts the secret under the user's login and the agent
// never sees the ciphertext. On a host with no keychain (a server, a container),
// the vault falls back to a passphrase-sealed file: the credentials are encrypted
// with XChaCha20-Poly1305 under a key derived from a passphrase with Argon2id, so
// the file is useless without the passphrase. Either way the plaintext exists only
// inside the process, as a secret.Text, for as long as a request needs it.
//
// The honest limit: a bearer API key must be sent verbatim, so the process holds
// plaintext at the moment of the call. Encryption at rest defeats disk theft,
// backups, and other users; it does not defeat code already running as this user.
// Removing even that requires a broker that holds the key out of process, which is
// a separate, heavier tier.
package vault

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ionalpha/flynn/secret"
)

// DefaultService is the keychain service name Flynn stores credentials under.
const DefaultService = "flynn"

// Sentinel errors for the sealed-file fallback.
var (
	// ErrBadPassphrase reports a wrong passphrase or a tampered sealed file: the
	// authenticated decryption failed, so no plaintext is returned.
	ErrBadPassphrase = errors.New("vault: wrong passphrase or corrupted vault")
	// ErrNoPassphrase reports that the sealed-file fallback is needed but no
	// passphrase was supplied (no keychain, and nothing to unlock the file with).
	ErrNoPassphrase = errors.New("vault: a passphrase is required but none was supplied")
)

// Passphrase supplies the passphrase that unlocks the sealed-file fallback. It is
// called only when the OS keychain is unavailable. newVault is true when a vault
// file is being created, so an interactive implementation can confirm the new
// passphrase, and false when an existing file is being opened.
type Passphrase func(newVault bool) (secret.Text, error)

// Store is the credential vault. It implements secret.Source (Lookup) for the read
// path the provider uses, and adds Set and Delete for the management commands. It
// is safe for sequential use by a CLI; it is not designed for concurrent writers.
type Store struct {
	service string
	file    string
	kr      Keyring
	pass    Passphrase
}

// Option configures a Store.
type Option func(*Store)

// WithKeyring overrides the OS keychain backend, for tests or to disable it.
func WithKeyring(kr Keyring) Option { return func(s *Store) { s.kr = kr } }

// WithPassphrase sets how the sealed-file fallback obtains its passphrase. The
// default reads the FLYNN_VAULT_PASSPHRASE environment variable; a CLI supplies a
// terminal prompt instead.
func WithPassphrase(p Passphrase) Option { return func(s *Store) { s.pass = p } }

// WithService overrides the keychain service name (default DefaultService).
func WithService(name string) Option {
	return func(s *Store) {
		if name != "" {
			s.service = name
		}
	}
}

// New builds a Store. The sealed-file fallback lives under dir; the OS keychain is
// used when available. By default the fallback passphrase is read from the
// environment, so a non-interactive run can unlock a file-backed vault.
func New(dir string, opts ...Option) *Store {
	s := &Store{
		service: DefaultService,
		file:    filepath.Join(dir, "vault.sealed"),
		kr:      OSKeyring(),
		pass:    EnvPassphrase,
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

var _ secret.Source = (*Store)(nil)

// Lookup resolves a credential reference to its value, implementing secret.Source.
// It consults the keychain first; if the keychain is the active backend the file
// is not touched, so a desktop lookup never prompts for a passphrase. Only when the
// keychain is unavailable does it open the sealed file. An absent credential
// returns secret.ErrNotFound.
func (s *Store) Lookup(_ context.Context, ref string) (secret.Text, error) {
	v, err := s.kr.Get(s.service, ref)
	switch {
	case err == nil:
		return secret.New(v), nil
	case errors.Is(err, ErrKeyNotFound):
		// Keychain is the active store and has no such entry.
		return secret.Text{}, secret.ErrNotFound
	}
	// Keychain unavailable: fall back to the sealed file.
	m, err := s.loadFile()
	if err != nil {
		return secret.Text{}, err
	}
	if val, ok := m[ref]; ok {
		return secret.New(val), nil
	}
	return secret.Text{}, secret.ErrNotFound
}

// Set stores a credential. It writes to the keychain when available (the OS
// encrypts it); otherwise it adds the value to the passphrase-sealed file. The
// value is taken as a secret.Text and exposed only to hand it to the backend.
func (s *Store) Set(_ context.Context, ref string, value secret.Text) error {
	if err := s.kr.Set(s.service, ref, value.Expose()); err == nil {
		return nil
	}
	// Keychain unavailable: seal into the file.
	m, err := s.loadFile()
	if err != nil {
		return err
	}
	if m == nil {
		m = make(map[string]string)
	}
	m[ref] = value.Expose()
	return s.saveFile(m)
}

// Delete removes a credential from both backends, so a stale sealed-file copy
// cannot resurrect a value removed from the keychain. Removing an absent
// credential is not an error.
func (s *Store) Delete(_ context.Context, ref string) error {
	_ = s.kr.Delete(s.service, ref)
	if !s.fileExists() {
		return nil
	}
	m, err := s.loadFile()
	if err != nil {
		return err
	}
	if _, ok := m[ref]; !ok {
		return nil
	}
	delete(m, ref)
	return s.saveFile(m)
}

// --- sealed-file backend ----------------------------------------------------

func (s *Store) fileExists() bool {
	_, err := os.Stat(s.file)
	return err == nil
}

// loadFile decrypts and returns the sealed credential map, or an empty map if no
// file exists yet. It obtains the passphrase through the configured Passphrase.
func (s *Store) loadFile() (map[string]string, error) {
	blob, err := os.ReadFile(s.file)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("vault: read sealed file: %w", err)
	}
	pass, err := s.passphrase(false)
	if err != nil {
		return nil, err
	}
	plain, err := open(blob, pass)
	if err != nil {
		return nil, err
	}
	var m map[string]string
	if err := json.Unmarshal(plain, &m); err != nil {
		return nil, fmt.Errorf("vault: malformed vault contents: %w", err)
	}
	return m, nil
}

// saveFile seals the credential map and writes it atomically with owner-only
// permissions, so the ciphertext never lands with looser access even briefly.
func (s *Store) saveFile(m map[string]string) error {
	plain, err := json.Marshal(m)
	if err != nil {
		return err
	}
	pass, err := s.passphrase(!s.fileExists())
	if err != nil {
		return err
	}
	blob, err := seal(plain, pass)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.file), 0o700); err != nil {
		return err
	}
	tmp := s.file + ".tmp"
	if err := os.WriteFile(tmp, blob, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.file)
}

// passphrase obtains the sealed-file passphrase as raw bytes, erroring if none is
// available.
func (s *Store) passphrase(newVault bool) ([]byte, error) {
	if s.pass == nil {
		return nil, ErrNoPassphrase
	}
	p, err := s.pass(newVault)
	if err != nil {
		return nil, err
	}
	if p.Empty() {
		return nil, ErrNoPassphrase
	}
	return []byte(p.Expose()), nil
}

// EnvPassphrase reads the sealed-file passphrase from FLYNN_VAULT_PASSPHRASE. It is
// the default so a non-interactive run can unlock a file-backed vault; it returns
// ErrNoPassphrase when the variable is unset.
func EnvPassphrase(bool) (secret.Text, error) {
	v, ok := os.LookupEnv("FLYNN_VAULT_PASSPHRASE")
	if !ok || v == "" {
		return secret.Text{}, ErrNoPassphrase
	}
	return secret.New(v), nil
}

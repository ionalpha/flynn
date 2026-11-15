// Package vault is Flynn's credential store: a secret.Source that holds API keys
// encrypted at rest, so a key is entered once, never written in plaintext, never
// echoed, and revealed only at the point of use (the model request's authorization
// header).
//
// Two backends are tried in order. The OS keychain is preferred, because the platform
// encrypts under the user's login and the agent never sees the ciphertext. A host with
// no keychain (a server, a container) uses a file sealed with XChaCha20-Poly1305 under
// an Argon2id key derived from a passphrase. Either way the plaintext exists only in
// the process, as a secret.Text, while a request needs it.
//
// Limit: a bearer key is sent verbatim, so the process holds plaintext during the
// call. Encryption at rest defeats disk theft, backups and other users, and does not
// defeat code running as this user; that needs an out-of-process broker.
package vault

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ionalpha/flynn/internal/fsatomic"
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

// Passphrase supplies the passphrase for the sealed-file fallback, called only when the
// OS keychain is unavailable. newVault is true when a file is being created, so an
// interactive implementation can confirm the new passphrase.
type Passphrase func(newVault bool) (secret.Text, error)

// Store is the credential vault: secret.Source (Lookup) for the provider's read path,
// plus Set and Delete for the management commands. It supports sequential use by a CLI
// and no concurrent writers.
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

// WithPassphrase sets how the sealed-file fallback obtains its passphrase. The default
// reads FLYNN_VAULT_PASSPHRASE; a CLI supplies a terminal prompt.
func WithPassphrase(p Passphrase) Option { return func(s *Store) { s.pass = p } }

// WithService overrides the keychain service name (default DefaultService).
func WithService(name string) Option {
	return func(s *Store) {
		if name != "" {
			s.service = name
		}
	}
}

// New builds a Store with the sealed-file fallback under dir. The default passphrase
// comes from the environment, so a non-interactive run can unlock a file-backed vault.
// FLYNN_VAULT_FILE forces the sealed file and never touches the OS keychain; an
// explicit WithKeyring still overrides it, because options apply after the default is
// chosen.
func New(dir string, opts ...Option) *Store {
	s := &Store{
		service: DefaultService,
		file:    filepath.Join(dir, "vault.sealed"),
		kr:      defaultKeyring(),
		pass:    EnvPassphrase,
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

func defaultKeyring() Keyring {
	if forceFileVault() {
		return disabledKeyring{}
	}
	return OSKeyring()
}

// forceFileVault reports whether FLYNN_VAULT_FILE (1, true, on or yes) asks for the
// sealed file only. The passphrase still comes from FLYNN_VAULT_PASSPHRASE or the CLI
// prompt.
func forceFileVault() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("FLYNN_VAULT_FILE"))) {
	case "1", "true", "on", "yes":
		return true
	default:
		return false
	}
}

var _ secret.Source = (*Store)(nil)

// Lookup resolves a credential reference, implementing secret.Source. When the
// keychain is the active backend the file is not touched, so a desktop lookup never
// prompts for a passphrase. An absent credential returns secret.ErrNotFound.
func (s *Store) Lookup(_ context.Context, ref string) (secret.Text, error) {
	v, err := s.kr.Get(s.service, ref)
	switch {
	case err == nil:
		return secret.New(v), nil
	case errors.Is(err, ErrKeyNotFound):
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

// Set stores a credential in the keychain when available, otherwise in the sealed
// file. The secret.Text is exposed only to hand it to the backend.
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

// Delete removes a credential from both backends, so a stale sealed-file copy cannot
// resurrect a value removed from the keychain. Removing an absent credential is not an
// error.
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

func (s *Store) fileExists() bool {
	_, err := os.Stat(s.file)
	return err == nil
}

// loadFile decrypts the sealed credential map, or returns an empty map when no file
// exists yet.
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

// saveFile seals the map and writes it atomically with owner-only permissions, so the
// ciphertext never exists with looser access, even briefly.
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
	return fsatomic.WriteFile(s.file, blob, 0o600)
}

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

// EnvPassphrase reads the sealed-file passphrase from FLYNN_VAULT_PASSPHRASE, or
// returns ErrNoPassphrase when it is unset.
func EnvPassphrase(bool) (secret.Text, error) {
	v, ok := os.LookupEnv("FLYNN_VAULT_PASSPHRASE")
	if !ok || v == "" {
		return secret.Text{}, ErrNoPassphrase
	}
	return secret.New(v), nil
}

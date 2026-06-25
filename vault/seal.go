package vault

import (
	//nolint:depguard // cryptographic salt/nonce for at-rest sealing; this entropy never enters the event stream, so the deterministic-replay rule does not apply
	"crypto/rand"
	"encoding/json"
	"fmt"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"
)

// sealFormat is the on-disk shape of a passphrase-sealed blob. It is self
// describing: the KDF parameters travel with the ciphertext so a vault written by
// one version still opens after the defaults are tuned, and the version guards a
// future format change. The salt and nonce are random per seal, so re-sealing the
// same value never produces the same bytes.
type sealFormat struct {
	Version    int    `json:"v"`
	Time       uint32 `json:"t"`    // argon2id passes
	MemoryKiB  uint32 `json:"m"`    // argon2id memory in KiB
	Threads    uint8  `json:"p"`    // argon2id parallelism
	Salt       []byte `json:"salt"` // argon2id salt
	Nonce      []byte `json:"n"`    // XChaCha20-Poly1305 nonce
	Ciphertext []byte `json:"c"`    // sealed payload (AEAD includes the auth tag)
}

// Argon2id parameters. The memory and time cost target a few hundred milliseconds
// on a desktop, the OWASP-style balance for an interactive unlock, high enough to
// make a stolen-vault dictionary attack expensive without a noticeable pause.
const (
	sealVersion    = 1
	argonTime      = 3
	argonMemoryKiB = 64 * 1024 // 64 MiB
	argonThreads   = 4
	argonKeyLen    = chacha20poly1305.KeySize
	saltLen        = 16
)

// seal encrypts plaintext under a key derived from passphrase with Argon2id, using
// XChaCha20-Poly1305 for authenticated encryption. The result is a JSON envelope
// carrying everything open needs except the passphrase.
func seal(plaintext, passphrase []byte) ([]byte, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("vault: salt: %w", err)
	}
	key := argon2.IDKey(passphrase, salt, argonTime, argonMemoryKiB, argonThreads, argonKeyLen)
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, fmt.Errorf("vault: cipher: %w", err)
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("vault: nonce: %w", err)
	}
	ciphertext := aead.Seal(nil, nonce, plaintext, nil)
	return json.Marshal(sealFormat{
		Version:    sealVersion,
		Time:       argonTime,
		MemoryKiB:  argonMemoryKiB,
		Threads:    argonThreads,
		Salt:       salt,
		Nonce:      nonce,
		Ciphertext: ciphertext,
	})
}

// open reverses seal. A wrong passphrase or any tampering with the blob fails the
// AEAD authentication and returns ErrBadPassphrase rather than partial plaintext,
// so a corrupted or brute-forced vault is rejected, never half-decrypted.
func open(blob, passphrase []byte) ([]byte, error) {
	var f sealFormat
	if err := json.Unmarshal(blob, &f); err != nil {
		return nil, fmt.Errorf("vault: malformed sealed blob: %w", err)
	}
	if f.Version != sealVersion {
		return nil, fmt.Errorf("vault: unsupported seal version %d", f.Version)
	}
	key := argon2.IDKey(passphrase, f.Salt, f.Time, f.MemoryKiB, f.Threads, argonKeyLen)
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, fmt.Errorf("vault: cipher: %w", err)
	}
	if len(f.Nonce) != aead.NonceSize() {
		return nil, ErrBadPassphrase
	}
	plaintext, err := aead.Open(nil, f.Nonce, f.Ciphertext, nil)
	if err != nil {
		return nil, ErrBadPassphrase
	}
	return plaintext, nil
}

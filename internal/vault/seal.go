package vault

import (
	//nolint:depguard // cryptographic salt/nonce for at-rest sealing; this entropy never enters the event stream, so the deterministic-replay rule does not apply
	"crypto/rand"
	"encoding/json"
	"fmt"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"
)

// sealFormat is the on-disk shape of a sealed blob. The KDF parameters travel with the
// ciphertext, so a vault still opens after the defaults are tuned, and the version
// guards a future format change. Salt and nonce are random per seal, so re-sealing a
// value never repeats bytes.
type sealFormat struct {
	Version    int    `json:"v"`
	Time       uint32 `json:"t"`    // argon2id passes
	MemoryKiB  uint32 `json:"m"`    // argon2id memory in KiB
	Threads    uint8  `json:"p"`    // argon2id parallelism
	Salt       []byte `json:"salt"` // argon2id salt
	Nonce      []byte `json:"n"`    // XChaCha20-Poly1305 nonce
	Ciphertext []byte `json:"c"`    // sealed payload (AEAD includes the auth tag)
}

// Argon2id parameters, targeting a few hundred milliseconds on a desktop: an
// interactive unlock without a noticeable pause, and an expensive dictionary attack on
// a stolen vault.
const (
	sealVersion    = 1
	argonTime      = 3
	argonMemoryKiB = 64 * 1024 // 64 MiB
	argonThreads   = 4
	argonKeyLen    = chacha20poly1305.KeySize
	saltLen        = 16
)

// Bounds on the KDF parameters read from a sealed blob. open reads them before the
// AEAD authenticates anything, so whoever edits the blob controls them. Unbounded, a
// crafted blob could demand a multi-gigabyte Argon2id allocation or a zero time or
// parallelism that panics the KDF. The default memory cost (64 MiB) sits well under
// the 1 GiB cap.
const (
	maxArgonTime      = 16
	maxArgonMemoryKiB = 1 << 20 // 1 GiB
	maxArgonThreads   = 64
	maxSaltLen        = 1 << 10 // 1 KiB; the default salt is 16 bytes
)

// seal encrypts plaintext with XChaCha20-Poly1305 under a key derived from passphrase
// with Argon2id. The JSON envelope carries everything open needs except the
// passphrase.
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

// open reverses seal. A wrong passphrase or any tampering fails AEAD authentication
// and returns ErrBadPassphrase, never partial plaintext.
func open(blob, passphrase []byte) ([]byte, error) {
	var f sealFormat
	if err := json.Unmarshal(blob, &f); err != nil {
		return nil, fmt.Errorf("vault: malformed sealed blob: %w", err)
	}
	if f.Version != sealVersion {
		return nil, fmt.Errorf("vault: unsupported seal version %d", f.Version)
	}
	// The bounds are checked before any allocation, so a hostile blob cannot exhaust
	// memory or panic the KDF on its way to the authentication failure. To the caller a
	// tampered blob looks like a wrong passphrase.
	if !f.validKDFParams() {
		return nil, ErrBadPassphrase
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

// validKDFParams reports whether a blob's Argon2id parameters are in bounds. Argon2id
// requires a time cost and parallelism of at least one and eight memory blocks per
// lane; the upper bounds and the salt bound cap resource use before the AEAD rejects a
// crafted blob.
func (f sealFormat) validKDFParams() bool {
	if f.Time < 1 || f.Time > maxArgonTime {
		return false
	}
	if f.Threads < 1 || f.Threads > maxArgonThreads {
		return false
	}
	if f.MemoryKiB > maxArgonMemoryKiB || f.MemoryKiB < 8*uint32(f.Threads) {
		return false
	}
	if len(f.Salt) < 8 || len(f.Salt) > maxSaltLen {
		return false
	}
	return true
}

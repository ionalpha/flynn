package vault

// Sealing and opening at the edges. A blob is refused unless it is well formed all the
// way through, because a partially trusted blob is one an attacker gets to shape. KDF
// parameters are read from the blob rather than assumed, so a vault sealed under older
// parameters still opens, but only within the bounds this build will accept.

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"
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

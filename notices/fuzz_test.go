package notices_test

import (
	"crypto/ed25519"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ionalpha/flynn/notices"
)

// FuzzVerify feeds arbitrary bytes to the verifier, which is the exact position a hostile
// origin (or anyone who can answer for one) is in. The property is absolute: no input
// makes it panic, and no input that is not a document we signed comes back accepted.
//
// This is the parser at the network boundary, so it is fuzzed on principle rather than
// because a bug is suspected: the parsers that most needed fuzzing have always been the
// ones nobody thought needed it.
func FuzzVerify(f *testing.F) {
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i + 7)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	signer, err := notices.NewSigner("fuzz-key", priv)
	if err != nil {
		f.Fatalf("signer: %v", err)
	}
	ring := notices.NewKeyring()
	if err := ring.Add("fuzz-key", priv.Public().(ed25519.PublicKey)); err != nil {
		f.Fatalf("keyring: %v", err)
	}
	now := time.Unix(1_780_000_000, 0).UTC()

	good, err := signer.Sign(notices.Feed{
		Version: 3,
		Issued:  now,
		Expires: now.Add(24 * time.Hour),
		Notices: []notices.Notice{{
			ID:       "FLYNN-2026-0001",
			Severity: notices.Security,
			Summary:  "the sandbox admitted a command it should have refused",
			FixedIn:  "0.1.4",
		}},
	})
	if err != nil {
		f.Fatalf("sign: %v", err)
	}

	f.Add(good)
	f.Add([]byte{})
	f.Add([]byte{0xd2, 0x84}) // a COSE tag and nothing else
	// A valid document with one byte corrupted: the shape that is closest to passing.
	corrupt := make([]byte, len(good))
	copy(corrupt, good)
	corrupt[len(corrupt)/2] ^= 0x01
	f.Add(corrupt)

	f.Fuzz(func(t *testing.T, doc []byte) {
		feed, err := notices.Verify(doc, ring)
		if err != nil {
			return
		}
		// Anything that verified must be exactly what we would have published: bounded,
		// well formed, and carrying nothing a terminal would act on.
		if len(feed.Notices) > notices.MaxNotices {
			t.Fatalf("a verified feed carried %d notices, over the cap", len(feed.Notices))
		}
		seen := map[string]bool{}
		for _, n := range feed.Notices {
			if !n.Severity.Valid() {
				t.Fatalf("a verified feed carried an unknown severity %q", n.Severity)
			}
			if n.ID == "" || n.Summary == "" {
				t.Fatalf("a verified feed carried an empty id or summary: %+v", n)
			}
			if seen[n.ID] {
				t.Fatalf("a verified feed carried a duplicate id %q", n.ID)
			}
			seen[n.ID] = true

			for _, s := range []string{n.ID, n.Summary, n.Detail, n.URL} {
				if !utf8.ValidString(s) {
					t.Fatalf("a verified feed carried invalid UTF-8: %q", s)
				}
				for _, r := range s {
					if r == '\n' || r == '\t' {
						continue
					}
					if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
						t.Fatalf("a verified feed carried a control character (%U) in %q", r, s)
					}
				}
			}
		}
	})
}

// FuzzSanitize checks the terminal-safety property directly against arbitrary bytes, so
// the guarantee is exercised even on inputs that could never survive CBOR's own UTF-8
// validation on the way in.
func FuzzSanitize(f *testing.F) {
	f.Add("plain text")
	f.Add("\x1b[2J\x1b[1;1H")
	f.Add("\u009b31m")
	f.Add("\xff\xfe")

	f.Fuzz(func(t *testing.T, s string) {
		out := notices.Sanitize(s, notices.MaxSummary)
		if !utf8.ValidString(out) {
			t.Fatalf("sanitize produced invalid UTF-8 from %q", s)
		}
		if n := utf8.RuneCountInString(out); n > notices.MaxSummary {
			t.Fatalf("sanitize returned %d runes, over the cap", n)
		}
		for _, r := range out {
			if r == '\n' || r == '\t' {
				continue
			}
			if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
				t.Fatalf("a control character (%U) survived sanitizing of %q", r, s)
			}
		}
	})
}

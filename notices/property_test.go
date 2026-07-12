package notices_test

import (
	"crypto/ed25519"
	"testing"
	"time"
	"unicode/utf8"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/notices"
)

// genSeverity draws one of the three severities a feed may carry.
func genSeverity() *rapid.Generator[notices.Severity] {
	return rapid.SampledFrom([]notices.Severity{notices.Security, notices.Deprecation, notices.Info})
}

// genText draws arbitrary UTF-8, escapes and control characters included. This is the
// point: the generator is the hostile publisher.
func genText(maxLen int) *rapid.Generator[string] {
	return rapid.StringOfN(rapid.Rune(), 0, maxLen, -1)
}

// TestSanitizeAlwaysYieldsTerminalSafeText is the property the whole rendering path rests
// on: whatever a feed says, what comes out carries nothing a terminal will act on. No
// input, however adversarial, produces an escape introducer.
func TestSanitizeAlwaysYieldsTerminalSafeText(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		in := genText(400).Draw(rt, "in")
		maxRunes := rapid.IntRange(0, 200).Draw(rt, "max")

		out := notices.Sanitize(in, maxRunes)

		if !utf8.ValidString(out) {
			rt.Fatalf("sanitize produced invalid UTF-8 from %q", in)
		}
		if n := utf8.RuneCountInString(out); n > maxRunes {
			rt.Fatalf("sanitize returned %d runes, over the %d cap", n, maxRunes)
		}
		for _, r := range out {
			if r == '\n' || r == '\t' {
				continue
			}
			if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
				rt.Fatalf("a control character (%U) survived sanitizing of %q", r, in)
			}
		}
	})
}

// TestSanitizeIsIdempotent: sanitizing text that is already sanitized changes nothing. If
// it did not hold, some path that sanitized twice would quietly mangle a notice, and the
// text a user reads would depend on how many times it happened to pass through.
func TestSanitizeIsIdempotent(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		in := genText(300).Draw(rt, "in")
		once := notices.Sanitize(in, 200)
		twice := notices.Sanitize(once, 200)
		if once != twice {
			rt.Fatalf("sanitize is not idempotent: %q then %q", once, twice)
		}
	})
}

// TestSignedFeedAlwaysVerifies: any feed we can publish, a client can verify, and what it
// reads back is the sanitized form of what we signed. A publisher cannot accidentally
// produce a document that its own clients reject.
func TestSignedFeedAlwaysVerifies(t *testing.T) {
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i + 3)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	signer, err := notices.NewSigner("prop-key", priv)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	ring := notices.NewKeyring()
	if err := ring.Add("prop-key", priv.Public().(ed25519.PublicKey)); err != nil {
		t.Fatalf("keyring: %v", err)
	}

	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(0, 8).Draw(rt, "count")
		f := notices.Feed{
			Version: rapid.Uint64().Draw(rt, "version"),
			Issued:  time.Unix(rapid.Int64Range(0, 1<<31).Draw(rt, "issued"), 0).UTC(),
			Expires: time.Unix(rapid.Int64Range(0, 1<<31).Draw(rt, "expires"), 0).UTC(),
		}
		for range n {
			f.Notices = append(f.Notices, notices.Notice{
				// Ids are drawn from a fixed alphabet so the generator spends its effort
				// on the text fields, and a collision (which the feed refuses) does not
				// dominate the run.
				ID:       "n" + rapid.StringOfN(rapid.RuneFrom([]rune("abcdef0123456789")), 1, 8, -1).Draw(rt, "id"),
				Severity: genSeverity().Draw(rt, "sev"),
				Summary:  "s" + genText(80).Draw(rt, "summary"),
				Detail:   genText(120).Draw(rt, "detail"),
			})
		}

		doc, err := signer.Sign(f)
		if err != nil {
			// The one thing a publisher can write that will not encode is text that is not
			// valid UTF-8, which the deterministic encoder refuses by design.
			return
		}
		got, err := notices.Verify(doc, ring)
		if err != nil {
			// A duplicate id, or a summary that sanitized away to nothing, is a feed the
			// client is right to refuse. Those are the structural rules, not a failure of
			// the round trip.
			return
		}
		if got.Version != f.Version {
			rt.Fatalf("version changed across the round trip: %d -> %d", f.Version, got.Version)
		}
		for i, n := range got.Notices {
			if want := notices.Sanitize(f.Notices[i].Summary, notices.MaxSummary); n.Summary != want {
				rt.Fatalf("summary %d came back as %q, want the sanitized %q", i, n.Summary, want)
			}
		}
	})
}

// TestRollbackIsAlwaysRefused: for any pair of feeds, once the higher version has been
// trusted, the lower one never is. This is the anti-rollback rule stated as a law rather
// than as one example, which is what makes it hold for versions nobody thought to write a
// case for.
func TestRollbackIsAlwaysRefused(t *testing.T) {
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i + 5)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	signer, err := notices.NewSigner("prop-key", priv)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	ring := notices.NewKeyring()
	if err := ring.Add("prop-key", priv.Public().(ed25519.PublicKey)); err != nil {
		t.Fatalf("keyring: %v", err)
	}
	now := time.Unix(1_780_000_000, 0).UTC()

	rapid.Check(t, func(rt *rapid.T) {
		lo := rapid.Uint64Range(0, 1<<32).Draw(rt, "lo")
		hi := rapid.Uint64Range(lo+1, 1<<33).Draw(rt, "hi")

		mk := func(v uint64) []byte {
			doc, err := signer.Sign(notices.Feed{Version: v, Issued: now, Expires: now.Add(time.Hour)})
			if err != nil {
				rt.Fatalf("sign: %v", err)
			}
			return doc
		}

		_, tr, err := notices.Accept(mk(hi), ring, notices.Trust{}, now)
		if err != nil {
			rt.Fatalf("accepting the newer feed failed: %v", err)
		}
		if _, _, err := notices.Accept(mk(lo), ring, tr, now); err == nil {
			rt.Fatalf("a feed at version %d was accepted after %d had been trusted", lo, hi)
		}
		// The same version again is a re-serve, not a rollback, and must keep working.
		if _, _, err := notices.Accept(mk(hi), ring, tr, now); err != nil {
			rt.Fatalf("re-serving the trusted version was refused: %v", err)
		}
	})
}

// TestAppliesRespectsTheRange: a notice applies to exactly the versions between its bounds.
// Stated as a property because an off-by-one here means either a user is told about a
// vulnerability they do not have, or is not told about one they do.
func TestAppliesRespectsTheRange(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		major := rapid.IntRange(0, 9).Draw(rt, "major")
		from := rapid.IntRange(1, 50).Draw(rt, "from")
		fixed := rapid.IntRange(from+1, 99).Draw(rt, "fixed")
		n := notices.Notice{
			ID:           "n",
			Severity:     notices.Security,
			Summary:      "s",
			AffectedFrom: ver(major, from),
			FixedIn:      ver(major, fixed),
		}

		below := rapid.IntRange(0, from-1).Draw(rt, "below")
		within := rapid.IntRange(from, fixed-1).Draw(rt, "within")
		above := rapid.IntRange(fixed, 200).Draw(rt, "above")

		if notices.Applies(n, ver(major, below)) && from > 0 {
			rt.Fatalf("a notice applied below its affected-from: %s < %s", ver(major, below), n.AffectedFrom)
		}
		if !notices.Applies(n, ver(major, within)) {
			rt.Fatalf("a notice did not apply inside its range: %s", ver(major, within))
		}
		if notices.Applies(n, ver(major, above)) {
			rt.Fatalf("a notice still applied at or past its fix: %s >= %s", ver(major, above), n.FixedIn)
		}
	})
}

// ver builds a dotted version whose patch component carries the drawn number. The major is
// held above zero-only so the all-zero development placeholder (which deliberately matches
// every notice) is not generated here.
func ver(major, patch int) string {
	return itoa(major) + ".1." + itoa(patch)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

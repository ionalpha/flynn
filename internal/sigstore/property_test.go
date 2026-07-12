package sigstore

import (
	"testing"

	"pgregory.net/rapid"
)

// TestPropertyNothingElseVerifies states the security property negatively: nothing a caller
// can construct makes this package say yes, except the genuine artifacts under the genuine
// pin.
//
// Verification is a gate whose only safe failure direction is closed. A bug that refuses a
// good release is noticed instantly, because nobody can install anything. A bug that accepts
// a bad one is noticed only after it has been exploited. So the property under test is the
// one that normal use would never reveal.
func TestPropertyNothingElseVerifies(t *testing.T) {
	t.Parallel()
	payload, sig, cert := load(t)

	t.Run("a modified payload never verifies", func(t *testing.T) {
		t.Parallel()
		rapid.Check(t, func(rt *rapid.T) {
			extra := rapid.SliceOfN(rapid.Byte(), 1, 64).Draw(rt, "extra")
			tampered := append(append([]byte(nil), payload...), extra...)
			if err := Verify(tampered, sig, cert, realIdentity()); err == nil {
				rt.Fatalf("a payload with %d extra bytes verified against the real signature", len(extra))
			}
		})
	})

	t.Run("an arbitrary signature never verifies", func(t *testing.T) {
		t.Parallel()
		rapid.Check(t, func(rt *rapid.T) {
			fake := rapid.SliceOfN(rapid.Byte(), 0, 128).Draw(rt, "sig")
			if err := Verify(payload, fake, cert, realIdentity()); err == nil {
				rt.Fatal("an arbitrary signature verified")
			}
		})
	})

	t.Run("an arbitrary certificate never verifies", func(t *testing.T) {
		t.Parallel()
		// The certificate arrives over the network beside the artifact, so an attacker
		// controls these bytes completely. "Arbitrary bytes" is the threat, not a
		// hypothetical.
		rapid.Check(t, func(rt *rapid.T) {
			fake := rapid.SliceOfN(rapid.Byte(), 0, 256).Draw(rt, "cert")
			if err := Verify(payload, sig, fake, realIdentity()); err == nil {
				rt.Fatal("an arbitrary certificate verified")
			}
		})
	})

	t.Run("only the pinned identity verifies", func(t *testing.T) {
		t.Parallel()
		// The release here is genuine. It is simply not the one the caller pinned, and
		// that alone must be enough to refuse it.
		rapid.Check(t, func(rt *rapid.T) {
			id := Identity{
				Workflow:   rapid.SampledFrom([]string{realWorkflow, "https://github.com/evil/x/.github/workflows/r.yml@refs/heads/main", ""}).Draw(rt, "workflow"),
				Issuer:     rapid.SampledFrom([]string{realIssuer, "https://evil.example/oidc", ""}).Draw(rt, "issuer"),
				SourceRepo: rapid.SampledFrom([]string{realSourceRepo, "evil/flynn-extensions", ""}).Draw(rt, "repo"),
			}
			err := Verify(payload, sig, cert, id)
			if id == realIdentity() {
				if err != nil {
					rt.Fatalf("the real release failed under its own pin: %v", err)
				}
				return
			}
			if err == nil {
				rt.Fatalf("identity %+v verified, but it is not the pinned one", id)
			}
		})
	})
}

// TestPropertyVerifyIsDeterministic: the same inputs always reach the same verdict. A gate
// that could flap is a gate an attacker simply retries.
func TestPropertyVerifyIsDeterministic(t *testing.T) {
	t.Parallel()
	payload, sig, cert := load(t)
	for range 32 {
		if err := Verify(payload, sig, cert, realIdentity()); err != nil {
			t.Fatalf("the real release failed to verify on a repeat run: %v", err)
		}
	}
}

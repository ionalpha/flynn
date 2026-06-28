package controlplane

import (
	"crypto/ed25519"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// fixedIdentity reconstructs an identity from a deterministic seed so a test pins exact
// derived names. The seed bytes are 0..31; a second instance uses 255..224.
func fixedIdentity(t *testing.T, reversed bool) *Identity {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		if reversed {
			seed[i] = byte(255 - i)
		} else {
			seed[i] = byte(i)
		}
	}
	id, err := IdentityFromSeed(seed)
	if err != nil {
		t.Fatalf("seed identity: %v", err)
	}
	return id
}

// TestExternalNameGolden pins the exact output of the derivation for known keys. The
// derivation is a frozen wire contract: a name registered with a provider must stay stable
// across releases, so any change that alters these values is a deliberate version bump, not
// an incidental edit. If this test fails, the algorithm changed and every previously
// derived name just moved.
func TestExternalNameGolden(t *testing.T) {
	id := fixedIdentity(t, false)
	cases := []struct {
		base, purpose string
		c             Constraints
		want          string
	}{
		{"flynn-agent", "fly-app", DNSName(30), "flynn-agent-fkhuncbv6jmjrxepg2"},
		// A larger budget yields the same name: the suffix is capped for readability.
		{"flynn-agent", "fly-app", DNSName(63), "flynn-agent-fkhuncbv6jmjrxepg2"},
		{"flynn", "dns", DNSName(20), "flynn-k0zmz9la8q20k7"},
		{"", "bucket", DNSName(40), "xnt3ya9i8ji8cndxc4"},
	}
	for _, tc := range cases {
		got, err := id.ExternalName(tc.base, tc.purpose, tc.c)
		if err != nil {
			t.Fatalf("ExternalName(%q,%q): %v", tc.base, tc.purpose, err)
		}
		if got != tc.want {
			t.Errorf("ExternalName(%q,%q,max=%d) = %q, want %q", tc.base, tc.purpose, tc.c.MaxLen, got, tc.want)
		}
		if err := tc.c.Validate(got); err != nil {
			t.Errorf("derived name %q does not satisfy its own constraints: %v", got, err)
		}
	}
}

// TestExternalNameDeterministic confirms the same identity, base, purpose, and constraints
// always derive the same name (so a redeploy targets the same resource).
func TestExternalNameDeterministic(t *testing.T) {
	id := fixedIdentity(t, false)
	a, err := id.ExternalName("flynn-agent", "fly-app", DNSName(30))
	if err != nil {
		t.Fatal(err)
	}
	b, err := id.ExternalName("flynn-agent", "fly-app", DNSName(30))
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("derivation not deterministic: %q != %q", a, b)
	}
}

// TestExternalNameDistinctIdentities confirms two different instances derive different names
// for the same resource, so names do not collide across the fleet.
func TestExternalNameDistinctIdentities(t *testing.T) {
	a, err := fixedIdentity(t, false).ExternalName("flynn-agent", "fly-app", DNSName(30))
	if err != nil {
		t.Fatal(err)
	}
	b, err := fixedIdentity(t, true).ExternalName("flynn-agent", "fly-app", DNSName(30))
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatalf("distinct identities derived the same name %q", a)
	}
}

// TestExternalNameDistinctPurposes confirms one identity derives different names for
// different resource roles, so an app, a bucket, and a record do not clobber one another.
func TestExternalNameDistinctPurposes(t *testing.T) {
	id := fixedIdentity(t, false)
	app, err := id.ExternalName("flynn", "fly-app", DNSName(30))
	if err != nil {
		t.Fatal(err)
	}
	bucket, err := id.ExternalName("flynn", "bucket", DNSName(30))
	if err != nil {
		t.Fatal(err)
	}
	if app == bucket {
		t.Fatalf("distinct purposes derived the same name %q", app)
	}
}

// TestExternalNamePrefixStable confirms the low-digit-first encoding is prefix-stable: for
// the same identity and purpose, a shorter suffix is a prefix of a longer one. This is what
// makes the derivation explainable and the cap a clean truncation rather than a reshuffle.
func TestExternalNamePrefixStable(t *testing.T) {
	id := fixedIdentity(t, false)
	// Two budgets that both leave room only in the suffix, below the readability cap, so the
	// suffix length tracks the budget.
	short, err := id.ExternalName("flynn", "dns", DNSName(18)) // suffix 12
	if err != nil {
		t.Fatal(err)
	}
	long, err := id.ExternalName("flynn", "dns", DNSName(20)) // suffix 14
	if err != nil {
		t.Fatal(err)
	}
	sSuf := strings.TrimPrefix(short, "flynn-")
	lSuf := strings.TrimPrefix(long, "flynn-")
	if !strings.HasPrefix(lSuf, sSuf) {
		t.Fatalf("suffix not prefix-stable: %q is not a prefix of %q", sSuf, lSuf)
	}
}

// TestResolveNameOverrideWins confirms an explicit name is used verbatim and reported as an
// override, and that an invalid override is rejected up front rather than at the provider.
func TestResolveNameOverrideWins(t *testing.T) {
	id := fixedIdentity(t, false)
	got, err := ResolveName(id, "flynn-agent", "fly-app", "my-own-app", DNSName(30))
	if err != nil {
		t.Fatal(err)
	}
	if got.Value != "my-own-app" || got.Source != NameOverride {
		t.Fatalf("override not honored: %+v", got)
	}
	if _, err := ResolveName(id, "flynn-agent", "fly-app", "Bad_Name", DNSName(30)); err == nil {
		t.Fatal("invalid override accepted")
	}
}

// TestResolveNameFallback confirms that with no identity in scope a name is still produced,
// derived from a throwaway identity and reported as ephemeral so the caller can see it will
// not be stable.
func TestResolveNameFallback(t *testing.T) {
	got, err := ResolveName(nil, "flynn-agent", "fly-app", "", DNSName(30))
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != NameEphemeral {
		t.Fatalf("expected ephemeral source, got %q", got.Source)
	}
	if err := DNSName(30).Validate(got.Value); err != nil {
		t.Fatalf("ephemeral name %q invalid: %v", got.Value, err)
	}
	// Two ephemeral draws should almost never match (random identity each time).
	other, _ := ResolveName(nil, "flynn-agent", "fly-app", "", DNSName(30))
	if got.Value == other.Value {
		t.Fatalf("two ephemeral names collided: %q", got.Value)
	}
}

// TestResolveNameIdentitySource confirms that with an identity in scope the derived name is
// reported as identity-sourced and matches the direct derivation.
func TestResolveNameIdentitySource(t *testing.T) {
	id := fixedIdentity(t, false)
	got, err := ResolveName(id, "flynn-agent", "fly-app", "", DNSName(30))
	if err != nil {
		t.Fatal(err)
	}
	want, _ := id.ExternalName("flynn-agent", "fly-app", DNSName(30))
	if got.Source != NameIdentity || got.Value != want {
		t.Fatalf("resolve mismatch: %+v want %q/identity", got, want)
	}
}

// TestExternalNameValidityProperty asserts the core invariant over arbitrary inputs: for any
// identity, base, purpose, and DNS-label budget, a successfully derived name satisfies the
// constraints (charset, length, leading letter, no edge separator). It must never emit a
// name a provider would reject; it may only refuse to derive one.
func TestExternalNameValidityProperty(t *testing.T) {
	baseChar := rapid.SampledFrom([]string{"a", "f", "z", "0", "9", "-", "flynn", "x"})
	rapid.Check(t, func(rt *rapid.T) {
		var seed [ed25519.SeedSize]byte
		copy(seed[:], rapid.SliceOfN(rapid.Byte(), ed25519.SeedSize, ed25519.SeedSize).Draw(rt, "seed"))
		id, err := IdentityFromSeed(seed[:])
		if err != nil {
			t.Fatal(err)
		}
		maxLen := rapid.IntRange(suffixFloorLen, 63).Draw(rt, "maxLen")
		c := DNSName(maxLen)
		// A base that may itself be invalid; the derivation must reject it, never emit junk.
		base := strings.Join(rapid.SliceOfN(baseChar, 0, 4).Draw(rt, "base"), "")
		purpose := rapid.StringN(0, 12, 12).Draw(rt, "purpose")

		name, err := id.ExternalName(base, purpose, c)
		if err != nil {
			return // refusing is allowed (e.g. base too long, or an invalid base)
		}
		if verr := c.Validate(name); verr != nil {
			t.Fatalf("derived %q for base=%q max=%d violates constraints: %v", name, base, maxLen, verr)
		}
	})
}

// TestExternalNameDeterminismProperty asserts determinism over arbitrary inputs: the same
// identity, base, purpose, and constraints always derive the same name.
func TestExternalNameDeterminismProperty(t *testing.T) {
	id := fixedIdentity(t, false)
	rapid.Check(t, func(rt *rapid.T) {
		base := rapid.SampledFrom([]string{"flynn", "flynn-agent", "a", ""}).Draw(rt, "base")
		purpose := rapid.StringN(0, 10, 10).Draw(rt, "purpose")
		maxLen := rapid.IntRange(suffixFloorLen+len(base)+1, 63).Draw(rt, "maxLen")
		c := DNSName(maxLen)
		a, errA := id.ExternalName(base, purpose, c)
		b, errB := id.ExternalName(base, purpose, c)
		if (errA == nil) != (errB == nil) || a != b {
			t.Fatalf("non-deterministic for base=%q purpose=%q max=%d: (%q,%v) vs (%q,%v)", base, purpose, maxLen, a, errA, b, errB)
		}
	})
}

// FuzzExternalName drives the derivation with unstructured inputs and asserts it never
// panics and never returns an invalid name: it either refuses or returns a name that
// satisfies the constraints.
func FuzzExternalName(f *testing.F) {
	f.Add([]byte("0123456789abcdef0123456789abcdef"), "flynn-agent", "fly-app", 30)
	f.Add([]byte("ffffffffffffffffffffffffffffffff"), "", "bucket", 12)
	f.Add(make([]byte, 32), "x", "", 63)
	f.Fuzz(func(t *testing.T, seedBytes []byte, base, purpose string, maxLen int) {
		if len(seedBytes) < ed25519.SeedSize {
			t.Skip()
		}
		id, err := IdentityFromSeed(seedBytes[:ed25519.SeedSize])
		if err != nil {
			t.Skip()
		}
		c := DNSName(maxLen)
		name, err := id.ExternalName(base, purpose, c)
		if err != nil {
			return
		}
		if verr := c.Validate(name); verr != nil {
			t.Fatalf("derived %q (base=%q purpose=%q max=%d) is invalid: %v", name, base, purpose, maxLen, verr)
		}
	})
}

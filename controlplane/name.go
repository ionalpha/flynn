package controlplane

// This file projects an instance identity into a name valid in an external provider's
// namespace. `PrincipalID` already renders the public key as the instance's internal,
// self-certifying id; an external resource name (a Fly app, a DNS record, an object-store
// bucket) is the same act against a namespace whose rules differ. Both are renderings of
// the one identity, so they live together.
//
// Why derive a name from the identity at all: many external systems require a name that is
// globally unique within a namespace shared by every user of the provider, and a redeploy
// must target the same resource rather than spawn a new one. Both fall out of the
// identity. The Ed25519 public key is globally unique (public keys do not collide) and
// stable across restarts, so a name encoding a hash of it is unique with no registry and
// identical every time the same instance asks for it. The determinism is the idempotency:
// `apps create <derived-name>` is a no-op on the second deploy because the name is the same.
//
// The derivation is a frozen wire contract. Once a derived name is registered with a
// provider, changing the algorithm would orphan that resource, so the version is bound
// into the hash domain (`externalNameDomain`) and pinned by golden vectors in the test. A
// future change is an explicit new version, never a silent reshaping of existing names.

import (
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"fmt"
	"math/big"
	"strings"
)

// Constraints describes a provider namespace's rules so one derivation serves every
// provider: the rules are data, not a per-provider code path. The derived suffix uses only
// `Charset`; the whole name is at least `MinLen` and at most `MaxLen`; when `LeadLetter` is
// set the name begins with an ASCII letter (the safest-everywhere lead, valid even where a
// leading digit is not). `Separator` is the literal joiner between the human-readable base
// and the derived suffix; it is legal in a name but never appears in the suffix, so a
// derived name can never carry a leading, trailing, or doubled separator.
type Constraints struct {
	Charset    string
	Separator  string
	MinLen     int
	MaxLen     int
	LeadLetter bool
}

// DNSName returns the constraints for an RFC 1123 DNS label capped at maxLen: lowercase
// alphanumerics joined by single hyphens, beginning with a letter. This one shape covers
// the common providers, whose names are all DNS labels: a Fly app (it becomes
// `<app>.fly.dev`), a DNS record, and a DNS-addressable object-store bucket. A provider's
// own length limit is the only thing that varies, so it is the single parameter; a caller
// that needs a different floor sets `MinLen` on the returned value.
func DNSName(maxLen int) Constraints {
	return Constraints{
		Charset:    "abcdefghijklmnopqrstuvwxyz0123456789",
		Separator:  "-",
		MinLen:     1,
		MaxLen:     maxLen,
		LeadLetter: true,
	}
}

// externalNameDomain is the domain-separation tag and version of the name derivation. It
// is mixed into the hash so a derived name can never collide with any other use of the
// public key, and so the algorithm is explicitly versioned: the derivation is a frozen
// contract (a registered name must stay stable), and a future change bumps this tag rather
// than silently altering every instance's name.
const externalNameDomain = "flynn/identity/external-name/v1"

// suffixFloorLen and suffixCapLen bound the derived suffix. The floor keeps enough entropy
// that distinct identities almost never collide (12 base-36 characters is about 62 bits);
// the cap keeps names readable while still carrying ample entropy (18 characters is about
// 93 bits, so a collision is negligible far past a trillion instances). The suffix fills
// the available length budget up to the cap.
const (
	suffixFloorLen = 12
	suffixCapLen   = 18
)

// ExternalName projects a public key into a name valid under c, beginning with the
// human-readable base and ending in a suffix derived from the key. The suffix is a hash of
// the key under this resource purpose, encoded into c.Charset, so two different identities
// almost never share a name and the same identity always derives the same one. It is the
// external-namespace companion to PrincipalID, which renders the same key as the internal
// id. base is the literal prefix that appears in the name (for example "flynn-agent");
// purpose names the resource role and feeds only the hash (for example "fly-app"), so one
// identity derives distinct names for distinct roles. It returns an error when base or the
// constraints cannot yield a valid name (for example a base too long for c.MaxLen).
func ExternalName(pub ed25519.PublicKey, base, purpose string, c Constraints) (string, error) {
	if len(pub) != ed25519.PublicKeySize {
		return "", fmt.Errorf("controlplane: external name: public key is %d bytes, want %d", len(pub), ed25519.PublicKeySize)
	}
	if err := c.valid(); err != nil {
		return "", err
	}
	// Domain-separated, length-unambiguous input: the fixed-length key comes last, after a
	// delimiter, so (purpose, key) maps injectively to the digest.
	h := sha256.New()
	h.Write([]byte(externalNameDomain))
	h.Write([]byte(purpose))
	h.Write([]byte{0x1f})
	h.Write(pub)
	return c.assemble(base, h.Sum(nil))
}

// ExternalName projects this identity into a name valid under c. It is the external-name
// companion to ID, mirroring how ExternalName(pub, ...) companions PrincipalID(pub): the
// identity owns both renderings of itself. See the package-level ExternalName for the
// meaning of base and purpose.
func (i *Identity) ExternalName(base, purpose string, c Constraints) (string, error) {
	return ExternalName(i.pub, base, purpose, c)
}

// NameSource records how ResolveName produced a name, so the choice is observable: an
// explicit name, a stable name derived from the instance identity, or an ephemeral name
// derived from a throwaway identity because none was in scope (the name will not be stable
// across runs).
type NameSource string

const (
	// NameOverride means the caller supplied the name verbatim.
	NameOverride NameSource = "override"
	// NameIdentity means the name was derived from the instance's stable identity.
	NameIdentity NameSource = "identity"
	// NameEphemeral means no identity was in scope, so the name was derived from a
	// throwaway identity and will differ on the next run.
	NameEphemeral NameSource = "ephemeral"
)

// ResolvedName is a name together with how it was chosen.
type ResolvedName struct {
	Value  string
	Source NameSource
}

// ResolveName is the single port every call site names an external resource through, so the
// override-then-identity-then-fallback policy is defined once rather than re-implemented per
// provider. An explicit override always wins (validated against c so a bad name fails fast
// rather than at the provider). Otherwise the name is derived from id. When id is nil no
// identity is in scope, so a throwaway identity is minted and used, and the result is marked
// NameEphemeral so the caller can see the name will not be stable. See ExternalName for base
// and purpose.
func ResolveName(id *Identity, base, purpose, override string, c Constraints) (ResolvedName, error) {
	if err := c.valid(); err != nil {
		return ResolvedName{}, err
	}
	if override != "" {
		if err := c.Validate(override); err != nil {
			return ResolvedName{}, fmt.Errorf("controlplane: name override %q is invalid: %w", override, err)
		}
		return ResolvedName{Value: override, Source: NameOverride}, nil
	}
	src := NameIdentity
	if id == nil {
		eph, err := GenerateIdentity()
		if err != nil {
			return ResolvedName{}, fmt.Errorf("controlplane: resolve name: %w", err)
		}
		id = eph
		src = NameEphemeral
	}
	name, err := id.ExternalName(base, purpose, c)
	if err != nil {
		return ResolvedName{}, err
	}
	return ResolvedName{Value: name, Source: src}, nil
}

// valid checks the constraint set itself is usable: a non-empty charset whose characters
// are all permitted in a name, a separator drawn from outside the charset (so the suffix
// can never produce one), and a length window that can hold a derived name.
func (c Constraints) valid() error {
	if c.Charset == "" {
		return errors.New("controlplane: name constraints: empty charset")
	}
	if c.MaxLen < c.MinLen {
		return fmt.Errorf("controlplane: name constraints: maxLen %d below minLen %d", c.MaxLen, c.MinLen)
	}
	if c.MaxLen < suffixFloorLen {
		return fmt.Errorf("controlplane: name constraints: maxLen %d below the %d-character suffix floor", c.MaxLen, suffixFloorLen)
	}
	if strings.ContainsAny(c.Charset, c.Separator) && c.Separator != "" {
		return fmt.Errorf("controlplane: name constraints: separator %q overlaps the charset", c.Separator)
	}
	if c.LeadLetter && leadingLetters(c.Charset) == "" {
		return errors.New("controlplane: name constraints: leadLetter set but charset has no leading letters")
	}
	return nil
}

// assemble builds the final name: the base, the separator, then a suffix encoded from the
// digest into the charset and sized to fill the remaining budget (between the entropy floor
// and the readability cap). The whole name is at most MaxLen by construction.
func (c Constraints) assemble(base string, digest []byte) (string, error) {
	prefix := base
	if base != "" && c.Separator != "" {
		prefix = base + c.Separator
	}
	if prefix != "" {
		if err := c.Validate(strings.TrimSuffix(prefix, c.Separator)); err != nil {
			return "", fmt.Errorf("controlplane: name base %q is invalid: %w", base, err)
		}
	}
	budget := c.MaxLen - len(prefix)
	need := suffixFloorLen
	if floor := c.MinLen - len(prefix); floor > need {
		need = floor
	}
	if need > budget {
		return "", fmt.Errorf("controlplane: base %q leaves %d characters, need at least %d for a derived name", base, budget, need)
	}
	n := budget
	if n > suffixCapLen {
		n = suffixCapLen
	}
	if n < need {
		n = need
	}
	suffix := encodeSuffix(digest, c.Charset, n)
	if prefix == "" && c.LeadLetter && !isASCIILetter(suffix[0]) {
		// No human prefix to guarantee the leading letter, so force the first suffix
		// character into the letter range using a digest byte not consumed by the encoding.
		letters := leadingLetters(c.Charset)
		b := []byte(suffix)
		b[0] = letters[int(digest[len(digest)-1])%len(letters)]
		suffix = string(b)
	}
	return prefix + suffix, nil
}

// encodeSuffix renders the digest as n characters of charset by reading it as a big-endian
// integer and emitting base-len(charset) digits, low digit first. Low-digit-first makes the
// encoding prefix-stable: a shorter suffix is a prefix of a longer one for the same digest.
// When the integer is exhausted the remaining positions take the charset's first character,
// so the length is always exactly n.
func encodeSuffix(digest []byte, charset string, n int) string {
	radix := big.NewInt(int64(len(charset)))
	x := new(big.Int).SetBytes(digest)
	mod := new(big.Int)
	out := make([]byte, n)
	for i := range n {
		x.DivMod(x, radix, mod)
		out[i] = charset[mod.Int64()]
	}
	return string(out)
}

// Validate reports whether name satisfies the constraints: every character is in the
// charset or is a separator, the length is within the window, the name begins with a letter
// when required, and it neither begins nor ends with a separator (the rule every DNS-label
// provider enforces). A caller's override is checked through this before it is used.
func (c Constraints) Validate(name string) error {
	if len(name) < c.MinLen || len(name) > c.MaxLen {
		return fmt.Errorf("length %d outside [%d, %d]", len(name), c.MinLen, c.MaxLen)
	}
	allowed := c.Charset + c.Separator
	for _, ch := range name {
		if !strings.ContainsRune(allowed, ch) {
			return fmt.Errorf("character %q not allowed", ch)
		}
	}
	if c.LeadLetter && len(name) > 0 && !isASCIILetter(name[0]) {
		return errors.New("must begin with a letter")
	}
	if c.Separator != "" {
		if strings.HasPrefix(name, c.Separator) || strings.HasSuffix(name, c.Separator) {
			return fmt.Errorf("must not begin or end with %q", c.Separator)
		}
	}
	return nil
}

// leadingLetters returns the run of ASCII letters at the start of charset, the characters a
// LeadLetter name may begin with.
func leadingLetters(charset string) string {
	i := 0
	for i < len(charset) && isASCIILetter(charset[i]) {
		i++
	}
	return charset[:i]
}

func isASCIILetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

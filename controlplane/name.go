package controlplane

import (
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"fmt"
	"math/big"
	"strings"
)

// Constraints describes a provider's naming rules as data, so one derivation serves
// every provider. The suffix uses only Charset; the name is MinLen to MaxLen long;
// LeadLetter requires an ASCII letter first. Separator joins base and suffix and is
// never in Charset, so a derived name cannot have a leading, trailing or doubled
// separator.
type Constraints struct {
	Charset    string
	Separator  string
	MinLen     int
	MaxLen     int
	LeadLetter bool
}

// DNSName returns the constraints of an RFC 1123 DNS label of at most maxLen:
// lowercase alphanumerics joined by single hyphens, starting with a letter. That
// covers a Fly app, a DNS record and a DNS-addressable bucket. Set MinLen on the
// result for a different floor.
func DNSName(maxLen int) Constraints {
	return Constraints{
		Charset:    "abcdefghijklmnopqrstuvwxyz0123456789",
		Separator:  "-",
		MinLen:     1,
		MaxLen:     maxLen,
		LeadLetter: true,
	}
}

// externalNameDomain is the domain-separation tag and version mixed into the name
// hash. A registered name must never change, so a new derivation needs a new tag;
// golden vectors in the tests pin this one.
const externalNameDomain = "flynn/identity/external-name/v1"

// suffixFloorLen and suffixCapLen bound the derived suffix: 12 base-36 characters is
// about 62 bits and 18 about 93, so a collision is negligible past a trillion
// instances. The suffix fills the length budget up to the cap.
const (
	suffixFloorLen = 12
	suffixCapLen   = 18
)

// ExternalName derives a name valid under c: base, then a suffix hashed from pub and
// purpose. A public key is unique and stable, so the name needs no registry and is
// the same on every redeploy, which makes creating the resource idempotent. base is
// the literal prefix (for example "flynn-agent"); purpose (for example "fly-app")
// feeds only the hash, so one identity gets a distinct name per role. It errors when
// base and c cannot yield a valid name.
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

// ExternalName is the package-level [ExternalName] for this identity's public key.
func (i *Identity) ExternalName(base, purpose string, c Constraints) (string, error) {
	return ExternalName(i.pub, base, purpose, c)
}

// NameSource records how [ResolveName] chose a name, so a caller can see when the name
// is ephemeral and will not be stable.
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

// ResolveName is the one place an external resource is named. A non-empty override
// wins, validated against c so a bad name fails here rather than at the provider.
// Otherwise the name derives from id; with id nil a throwaway identity is used and
// the result is marked NameEphemeral.
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

// valid checks c itself is usable: a non-empty charset of permitted characters, a
// separator outside the charset, and a length window that fits a derived name.
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

// assemble builds base, separator and a suffix sized between the entropy floor and
// the readability cap. The result never exceeds MaxLen.
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

// encodeSuffix renders the digest, read as a big-endian integer, as n base-len(charset)
// digits, low digit first, so a shorter suffix is a prefix of a longer one. Positions
// past the integer's digits take the charset's first character.
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

// Validate reports whether name satisfies c: only charset or separator characters, a
// length within the window, a leading letter when required, and no leading or
// trailing separator, as DNS-label providers require.
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

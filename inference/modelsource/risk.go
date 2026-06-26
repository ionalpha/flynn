package modelsource

import "github.com/ionalpha/flynn/sandbox"

// Integrity is what is known about a model's bytes: whether they have been verified
// against a digest, and how strong that pin is.
type Integrity int

const (
	// IntegrityUnverified means no digest has been established for the source yet, so
	// its bytes carry no integrity claim.
	IntegrityUnverified Integrity = iota
	// IntegrityTOFU means the digest was pinned on first use (trust on first use): a
	// later fetch that does not match is refused, but the first fetch trusted whoever
	// answered.
	IntegrityTOFU
	// IntegrityPinned means the digest was pinned ahead of time by a curator (a catalog
	// entry), the strongest claim: the bytes are exactly what was vetted offline.
	IntegrityPinned
)

// String names the integrity level in plain words.
func (i Integrity) String() string {
	switch i {
	case IntegrityPinned:
		return "verified against a pinned digest"
	case IntegrityTOFU:
		return "pinned on first use (lower trust)"
	default:
		return "unverified"
	}
}

// RiskSurface is the plain-language summary of what running a model means: how far its
// source is trusted, the isolation that trust requires, whether its bytes are verified,
// and its network posture. It is what a user is shown before a model is run, so the risk
// is visible without reading any documentation.
type RiskSurface struct {
	// Source is the original reference.
	Source string
	// Trust is the classified trust level.
	Trust sandbox.Trust
	// TrustReason is the plain-language reason for the trust level.
	TrustReason string
	// Required is the containment the trust level demands to run.
	Required sandbox.Containment
	// Integrity is what is known about the bytes.
	Integrity Integrity
	// Egress states the network posture of a local model run. A local model is served on
	// a loopback port and needs no outbound access, so egress is denied; surfacing it
	// tells the user a model cannot phone home.
	Egress string
}

// DescribeRisk builds the risk surface for a classified source. The containment comes
// from the trust level through the same gate the run uses, so what the user is shown is
// exactly what is enforced.
func DescribeRisk(src Source, class Classification, integrity Integrity) RiskSurface {
	return RiskSurface{
		Source:      src.Raw,
		Trust:       class.Trust,
		TrustReason: class.Reason,
		Required:    sandbox.Required(class.Trust),
		Integrity:   integrity,
		Egress:      "no network access (served on a loopback port)",
	}
}

// Lines renders the risk surface as plain-language lines for display. Each line is a
// single fact a non-expert can read: what the source is, how trusted, how isolated, how
// verified, and its network posture.
func (r RiskSurface) Lines() []string {
	return []string{
		"source:    " + r.Source,
		"trust:     " + r.Trust.String() + " (" + r.TrustReason + ")",
		"isolation: requires " + r.Required.String(),
		"integrity: " + r.Integrity.String(),
		"network:   " + r.Egress,
	}
}

// Risky reports whether running this source is a risk a user must explicitly accept: any
// source that is not a vetted, trusted catalog entry. A trusted source runs without a
// consent prompt; everything else is a deliberate choice.
func (r RiskSurface) Risky() bool { return r.Trust != sandbox.TrustTrusted }

package modelsource

import "github.com/ionalpha/flynn/sandbox"

// Integrity is what is known about a model's bytes: whether they were verified against
// a digest, and how strong that pin is.
type Integrity int

const (
	// IntegrityUnverified means no digest has been established for the source yet.
	IntegrityUnverified Integrity = iota
	// IntegrityTOFU means the digest was pinned on first use: a later mismatch is
	// refused, but the first fetch trusted whoever answered.
	IntegrityTOFU
	// IntegrityPinned means a curator pinned the digest ahead of time (a catalog entry),
	// so the bytes are exactly what was vetted offline.
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

// RiskSurface is what a user is shown before a model runs: how far its source is
// trusted, the isolation that requires, whether its bytes are verified, and its network
// posture.
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
	// Egress is the network posture. A local model is served on loopback and needs no
	// outbound access, so egress is denied and the user is told it cannot phone home.
	Egress string
}

// DescribeRisk builds the risk surface for a classified source. The containment comes
// from the same gate the run uses, so what the user is shown is what is enforced.
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

// Lines renders the risk surface as one plain-language fact per line.
func (r RiskSurface) Lines() []string {
	return []string{
		"source:    " + r.Source,
		"trust:     " + r.Trust.String() + " (" + r.TrustReason + ")",
		"isolation: requires " + r.Required.String(),
		"integrity: " + r.Integrity.String(),
		"network:   " + r.Egress,
	}
}

// Risky reports whether the user must accept the risk explicitly: any source that is
// not a trusted catalog entry.
func (r RiskSurface) Risky() bool { return r.Trust != sandbox.TrustTrusted }

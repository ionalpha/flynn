package learn

import (
	"context"
	"errors"
	"math"

	"github.com/ionalpha/flynn/state"
)

// wilsonZ is the z-score for a 95% confidence interval, used by the Wilson lower
// bound that ranks skills by evidence.
const wilsonZ = 1.96

// Confidence estimates how reliably a skill helps, as the Wilson lower bound of its
// win rate (wins over reads). The lower bound is the principled small-sample
// estimate: it is conservative when evidence is thin, so a skill that won its only
// read does not outrank one that won 50 of 55, and it rises toward the raw win
// rate as evidence accumulates. It is 0 when the skill has never been read.
//
// Grading a skill by its confirmed outcomes is what makes this learning loop
// self-correcting where a recency- or usage-only one is not: a skill that keeps
// being read in failing runs decays however often it is offered.
func Confidence(reads, wins int) float64 {
	if reads <= 0 || wins <= 0 {
		return 0
	}
	n := float64(reads)
	phat := float64(wins) / n
	z := wilsonZ
	denom := 1 + z*z/n
	centre := phat + z*z/(2*n)
	margin := z * math.Sqrt((phat*(1-phat)+z*z/(4*n))/n)
	lb := (centre - margin) / denom
	if lb < 0 {
		return 0
	}
	return lb
}

// Reinforce records one run's outcome against the skills the run read: each is read
// once more, and once more a win if the run succeeded. The evidence accrues on the
// skill so it can be ranked and retired by how it actually performs. A duplicate or
// empty reference, or one with no live skill, is skipped; the first store error is
// returned. It is a read-modify-write per skill, which is safe under the agent's
// single-writer local store.
//
// Pass the skills the run actually loaded, from the skill toolset's record of what
// it served, not the ones recall offered. An offer is a keyword match on an
// objective; it establishes nothing about whether the model took the skill up, and
// crediting a run's outcome to every skill in its prompt makes the win rate a
// property of the run rather than of the skill. Offer records that half.
//
// Pass ids, not slugs. Recall is scope-blind, so what it returns can be a bundled
// skill or a learned one, and two scopes may hold the same slug; Get resolves a slug
// by earliest created row, which would credit the run to whichever record happens to
// be older. An id names the record that was actually put in front of the model.
func Reinforce(ctx context.Context, skills state.SkillStore, ids []string, success bool) error {
	return accrue(ctx, skills, ids, func(sk *state.Skill) {
		sk.Reads++
		if success {
			sk.Wins++
		}
	})
}

// Offer records that a run was shown these skills, without claiming anything about
// what it did with them. It is the other half of Reinforce and takes the ids recall
// surfaced, so offered-and-never-read stays visible: a skill with many offers and no
// reads has a description that is not selling a body that might have helped, which is
// a defect in the writing rather than in the skill.
//
// Offers never feed Confidence or Decay. A skill is not worse for being offered to
// runs that turned out not to need it.
func Offer(ctx context.Context, skills state.SkillStore, ids []string) error {
	return accrue(ctx, skills, ids, func(sk *state.Skill) { sk.Offers++ })
}

// accrue applies bump to each distinct live skill named in ids and writes it back,
// skipping empty, duplicate and unknown references and returning the first store
// error. Both counters move through here so neither can drift on which references
// it honours.
func accrue(ctx context.Context, skills state.SkillStore, ids []string, bump func(*state.Skill)) error {
	seen := map[string]bool{}
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		sk, err := skills.Get(ctx, id)
		if errors.Is(err, state.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		bump(&sk)
		if _, err := skills.Upsert(ctx, sk); err != nil {
			return err
		}
	}
	return nil
}

// DecayPolicy decides when a skill is retired: once it has at least MinReads of
// evidence and its confidence is below MinConfidence, it has been tried enough and
// helped too rarely to keep surfacing.
//
// The threshold counts reads, so a skill is only ever retired over runs that loaded
// it. A skill that keeps being offered and never read is not retired by this policy:
// nothing has tried it, and the repair is to its description.
type DecayPolicy struct {
	MinReads      int
	MinConfidence float64
}

// DefaultDecay retires a skill only after a fair number of reads with a poor
// confirmed win rate, so a still-unproven skill keeps its chance.
func DefaultDecay() DecayPolicy { return DecayPolicy{MinReads: 5, MinConfidence: 0.2} }

// Decay archives the skills in scope that the policy judges unhelpful, returning
// the ones it archived. Archiving is a soft delete (a tombstone), so a retired
// skill is recoverable and never silently lost, and a skill with little evidence is
// kept because it has not yet earned retirement.
//
// The bundled scope is refused with ErrBundledScope: a skill shipped in the binary
// is replaced by an upgrade, not retired by a policy, and archiving one would leave
// a tombstone for the next seed to fight with.
func Decay(ctx context.Context, skills state.SkillStore, scope state.Scope, p DecayPolicy) ([]state.Skill, error) {
	if scope == state.BundledScope {
		return nil, ErrBundledScope
	}
	all, err := skills.List(ctx, scope)
	if err != nil {
		return nil, err
	}
	var archived []state.Skill
	for _, sk := range all {
		if sk.Reads >= p.MinReads && Confidence(sk.Reads, sk.Wins) < p.MinConfidence {
			if err := skills.Delete(ctx, sk.ID); err != nil {
				return archived, err
			}
			archived = append(archived, sk)
		}
	}
	return archived, nil
}

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
// win rate (wins over uses). The lower bound is the principled small-sample
// estimate: it is conservative when evidence is thin, so a skill that won its only
// use does not outrank one that won 50 of 55, and it rises toward the raw win rate
// as evidence accumulates. It is 0 when the skill has never been used.
//
// Grading a skill by its confirmed outcomes is what makes this learning loop
// self-correcting where a recency- or usage-only one is not: a skill that keeps
// appearing in failing runs decays however often it is touched.
func Confidence(uses, wins int) float64 {
	if uses <= 0 || wins <= 0 {
		return 0
	}
	n := float64(uses)
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

// Reinforce records one run's outcome against the skills it recalled: each is used
// once more, and once more a win if the run succeeded. The evidence accrues on the
// skill so it can be ranked and retired by how it actually performs. A duplicate or
// empty slug, or one with no live skill, is skipped; the first store error is
// returned. It is a read-modify-write per skill, which is safe under the agent's
// single-writer local store.
func Reinforce(ctx context.Context, skills state.SkillStore, slugs []string, success bool) error {
	seen := map[string]bool{}
	for _, slug := range slugs {
		if slug == "" || seen[slug] {
			continue
		}
		seen[slug] = true
		sk, err := skills.Get(ctx, slug)
		if errors.Is(err, state.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		sk.Uses++
		if success {
			sk.Wins++
		}
		if _, err := skills.Upsert(ctx, sk); err != nil {
			return err
		}
	}
	return nil
}

// DecayPolicy decides when a skill is retired: once it has at least MinUses of
// evidence and its confidence is below MinConfidence, it has been tried enough and
// helped too rarely to keep surfacing.
type DecayPolicy struct {
	MinUses       int
	MinConfidence float64
}

// DefaultDecay retires a skill only after a fair number of uses with a poor
// confirmed win rate, so a still-unproven skill keeps its chance.
func DefaultDecay() DecayPolicy { return DecayPolicy{MinUses: 5, MinConfidence: 0.2} }

// Decay archives the skills in scope that the policy judges unhelpful, returning
// the ones it archived. Archiving is a soft delete (a tombstone), so a retired
// skill is recoverable and never silently lost, and a skill with little evidence is
// kept because it has not yet earned retirement.
func Decay(ctx context.Context, skills state.SkillStore, scope state.Scope, p DecayPolicy) ([]state.Skill, error) {
	all, err := skills.List(ctx, scope)
	if err != nil {
		return nil, err
	}
	var archived []state.Skill
	for _, sk := range all {
		if sk.Uses >= p.MinUses && Confidence(sk.Uses, sk.Wins) < p.MinConfidence {
			if err := skills.Delete(ctx, sk.ID); err != nil {
				return archived, err
			}
			archived = append(archived, sk)
		}
	}
	return archived, nil
}

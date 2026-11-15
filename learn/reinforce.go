package learn

import (
	"context"
	"errors"
	"math"

	"github.com/ionalpha/flynn/state"
)

// wilsonZ is the z-score for a 95% confidence interval.
const wilsonZ = 1.96

// Confidence is the Wilson lower bound of wins over reads, in [0, 1], and 0 for an
// unread skill. The bound is low when evidence is thin, so a skill that won its only
// read ranks below one that won 50 of 55. A skill read in failing runs decays
// however often it is offered.
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

// Reinforce adds a read to each skill in ids, and a win if success. Empty, duplicate
// and unknown ids are skipped; the first store error is returned. Each update is a
// read-modify-write, safe only under a single-writer store.
//
// Pass the ids the skill toolset served. Crediting every offered skill would make
// the win rate measure the run; [Offer] records offers. Pass ids because two scopes
// may share a slug and Get resolves a slug by earliest created row.
func Reinforce(ctx context.Context, skills state.SkillStore, ids []string, success bool) error {
	return accrue(ctx, skills, ids, func(sk *state.Skill) {
		sk.Reads++
		if success {
			sk.Wins++
		}
	})
}

// Offer adds an offer to each skill recall surfaced. Many offers and no reads point
// at a weak description. Offers never feed [Confidence] or [Decay], because a skill
// is not worse for being offered to runs that did not need it.
func Offer(ctx context.Context, skills state.SkillStore, ids []string) error {
	return accrue(ctx, skills, ids, func(sk *state.Skill) { sk.Offers++ })
}

// accrue is shared by [Reinforce] and [Offer] so both skip the same ids.
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

// DecayPolicy retires a skill with at least MinReads reads and [Confidence] below
// MinConfidence. Only reads count, so a skill offered and never read is kept; its
// description needs the fix.
type DecayPolicy struct {
	MinReads      int
	MinConfidence float64
}

// DefaultDecay is 5 reads below 0.2 confidence.
func DefaultDecay() DecayPolicy { return DecayPolicy{MinReads: 5, MinConfidence: 0.2} }

// Decay archives the skills in scope that p retires and returns them. Archiving is a
// tombstone, so a retired skill is recoverable. state.BundledScope returns
// [ErrBundledScope] because an upgrade replaces a bundled skill and a tombstone
// would conflict with the next seed.
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

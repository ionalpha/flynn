package learn

import (
	"context"

	"github.com/ionalpha/flynn/state"
)

// RegradeResult is what a [Regrade] pass did.
type RegradeResult struct {
	Checked     int           // skills whose check ran
	Reconfirmed []state.Skill // passed, tagged verified
	Retired     []state.Skill // failed, archived
}

// Regrade re-runs every skill check in scope, so a procedure that stopped working is
// archived. A pass is tagged verified; a skill whose check cannot run is untouched.
// A nil v does nothing. The first store or verifier error is returned.
//
// state.BundledScope returns [ErrBundledScope]: bundled skills are versioned with the
// binary, and a check failing on one machine must not archive an authored skill.
func Regrade(ctx context.Context, skills state.SkillStore, scope state.Scope, v Verifier) (RegradeResult, error) {
	var res RegradeResult
	if scope == state.BundledScope {
		return res, ErrBundledScope
	}
	if v == nil {
		return res, nil
	}
	all, err := skills.List(ctx, scope)
	if err != nil {
		return res, err
	}
	for _, sk := range all {
		if sk.Check == "" {
			continue
		}
		verdict, err := v.Verify(ctx, Lesson{Kind: LessonSkill, Check: sk.Check}, scope)
		if err != nil {
			return res, err
		}
		if !verdict.Ran {
			continue
		}
		res.Checked++
		if verdict.Verified {
			sk.Tags = retagVerified(sk.Tags)
			updated, err := skills.Upsert(ctx, sk)
			if err != nil {
				return res, err
			}
			res.Reconfirmed = append(res.Reconfirmed, updated)
			continue
		}
		if err := skills.Delete(ctx, sk.ID); err != nil {
			return res, err
		}
		res.Retired = append(res.Retired, sk)
	}
	return res, nil
}

// retagVerified swaps [unverifiedTag] for [verifiedTag] and keeps other tags.
func retagVerified(tags []string) []string {
	out := make([]string, 0, len(tags)+1)
	verified := false
	for _, t := range tags {
		if t == unverifiedTag {
			continue
		}
		if t == verifiedTag {
			verified = true
		}
		out = append(out, t)
	}
	if !verified {
		out = append(out, verifiedTag)
	}
	return out
}

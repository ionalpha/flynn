package learn

import (
	"context"

	"github.com/ionalpha/flynn/state"
)

// RegradeResult summarizes a regrade pass: the skills whose checks were re-run and
// what became of them.
type RegradeResult struct {
	Checked     int           // skills with a check that were re-run
	Reconfirmed []state.Skill // checks re-ran and still passed
	Retired     []state.Skill // checks re-ran and now failed, so they were archived
}

// Regrade re-runs the verification check of every checkable skill in scope and
// brings the corpus back in line with current ground truth: a skill whose check
// still passes is re-confirmed (tagged verified), and one whose check now fails is
// retired (archived, recoverable). Skills with no check, or whose check could not
// run, are left untouched.
//
// This is what a write-once skill file cannot do: knowledge captured earlier is
// re-graded as the environment changes, so a procedure that has quietly stopped
// working is caught and removed rather than recalled forever. A nil verifier is a
// no-op; the first store or verifier error aborts and is returned.
func Regrade(ctx context.Context, skills state.SkillStore, scope state.Scope, v Verifier) (RegradeResult, error) {
	var res RegradeResult
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
		verdict, err := v.Verify(ctx, Lesson{Kind: LessonSkill, Check: sk.Check})
		if err != nil {
			return res, err
		}
		if !verdict.Ran {
			continue // could not run the check: leave the skill as it stands
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

// retagVerified ensures a re-confirmed skill is tagged verified and no longer
// unverified, preserving its other tags.
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

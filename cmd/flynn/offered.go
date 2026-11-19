package main

import (
	"context"

	"github.com/ionalpha/flynn/governed"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/state"
)

// skillSlugs names the skills behind ids, in order. An id the store no longer answers to
// is left out: the record names what was offered, and a skill that vanished between
// recall and the run was not.
func skillSlugs(ctx context.Context, skills state.SkillStore, ids []string) []string {
	var out []string
	for _, id := range ids {
		if sk, err := skills.Get(ctx, id); err == nil {
			out = append(out, sk.Slug)
		}
	}
	return out
}

// reportOffered writes one "offered" skill event per recalled skill. Recall runs before
// the run's stream exists, so the events are written once the goal is submitted.
func reportOffered(ctx context.Context, run *governed.Run, slugs []string) {
	rep := run.Session().Reporter()
	for _, slug := range slugs {
		rep.Report(ctx, mission.Event{Kind: mission.EventSkill, Skill: slug, Via: "offered", Result: "recalled into the prompt"})
	}
}

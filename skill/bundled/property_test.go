package bundled

import (
	"context"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/state"
)

// Property: whatever the store held before, reconciling leaves the bundled scope
// holding exactly the pack it was given, with each skill's content the pack's and
// its outcome evidence the store's, and nothing outside that scope touched.
//
// This is the whole of what an upgrade promises. Stating it as a property rather
// than as cases is the point: the interesting inputs are the overlaps between what
// an older binary seeded and what this one ships, and a released binary meets a set
// of overlaps nobody wrote a case for.
func TestProp_ReconcileMakesTheScopeExact(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		ctx := context.Background()
		slugs := rapid.StringMatching(`[a-z]{1,4}`)
		body := rapid.StringMatching(`[a-z ]{0,8}`)

		store := newStore(t)

		// What an older binary seeded, each with outcome evidence this machine earned.
		seeded := map[string]int{}
		for i, slug := range rapid.SliceOfN(slugs, 0, 5).Draw(rt, "seeded") {
			reads := rapid.IntRange(0, 9).Draw(rt, "reads")
			sk := bundledSkill(slug, body.Draw(rt, "seededBody"))
			sk.Offers, sk.Reads, sk.Wins = reads, reads, reads
			if _, err := store.Upsert(ctx, sk); err != nil {
				rt.Fatalf("seed %d: %v", i, err)
			}
			seeded[slug] = reads
		}

		// A skill of the user's own, in their own scope, sharing a slug with the pack
		// as often as the generator makes it so.
		mine, err := store.Upsert(ctx, state.Skill{Slug: slugs.Draw(rt, "mySlug"), Name: "mine", Body: "mine"})
		if err != nil {
			rt.Fatalf("upsert user skill: %v", err)
		}

		// What this binary ships. Deduplicated by slug, because a pack is directories
		// and two of them cannot share a name.
		want := map[string]state.Skill{}
		var order []string
		for _, slug := range rapid.SliceOfN(slugs, 0, 5).Draw(rt, "shipped") {
			if _, dup := want[slug]; dup {
				continue
			}
			want[slug] = bundledSkill(slug, body.Draw(rt, "shippedBody"))
			order = append(order, slug)
		}
		shipped := make([]state.Skill, 0, len(order))
		for _, slug := range order {
			shipped = append(shipped, want[slug])
		}

		if _, err := reconcile(ctx, store, shipped); err != nil {
			rt.Fatalf("reconcile: %v", err)
		}

		got, err := store.List(ctx, state.BundledScope)
		if err != nil {
			rt.Fatalf("list: %v", err)
		}
		if len(got) != len(want) {
			rt.Fatalf("bundled scope holds %d skills, pack ships %d", len(got), len(want))
		}
		for _, sk := range got {
			w, ok := want[sk.Slug]
			if !ok {
				rt.Fatalf("%s is in the bundled scope but not in the pack", sk.Slug)
			}
			if sk.Body != w.Body || sk.Description != w.Description || sk.Name != w.Name {
				rt.Errorf("%s: stored content is not the pack's", sk.Slug)
			}
			// Evidence survives an upgrade of a skill that was already there, and a
			// skill this pack introduces starts with none.
			if sk.Offers != seeded[sk.Slug] || sk.Reads != seeded[sk.Slug] || sk.Wins != seeded[sk.Slug] {
				rt.Errorf("%s: offers/reads/wins %d/%d/%d, want %d", sk.Slug, sk.Offers, sk.Reads, sk.Wins, seeded[sk.Slug])
			}
		}

		still, err := store.Get(ctx, mine.ID)
		if err != nil {
			rt.Fatalf("the user's own skill is gone: %v", err)
		}
		if still.Body != "mine" || still.Scope != (state.Scope{}) {
			rt.Errorf("the user's own skill changed: %+v", still)
		}
	})
}

// Property: after a pack update, no body a host wrote is lost. A skill the host
// edited to something other than what the pack now ships keeps the host's body, in
// the bundled scope if the pack still ships the skill and in the host's own scope if
// not; a skill the host did not edit holds the pack's new body or is gone with the
// pack's. A second run with the same pack changes nothing.
func TestProp_AHostEditSurvivesAPackUpdate(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		ctx := context.Background()
		slugs := rapid.StringMatching(`[a-z]{1,3}`)
		body := rapid.StringMatching(`[a-z]{1,3}`)
		store := newStore(t)

		v1 := map[string]string{}
		for _, slug := range rapid.SliceOfN(slugs, 1, 5).Draw(rt, "v1") {
			v1[slug] = body.Draw(rt, "v1Body")
		}
		reconcileMap := func(m map[string]string) Result {
			var pack []state.Skill
			for slug, b := range m {
				pack = append(pack, bundledSkill(slug, b))
			}
			res, err := reconcile(ctx, store, pack)
			if err != nil {
				rt.Fatalf("reconcile: %v", err)
			}
			return res
		}
		reconcileMap(v1)

		edited := map[string]string{}
		for slug := range v1 {
			if rapid.Bool().Draw(rt, "edit "+slug) {
				sk, err := store.Get(ctx, slug)
				if err != nil {
					rt.Fatalf("get %s: %v", slug, err)
				}
				sk.Body = body.Draw(rt, "hostBody")
				if _, err := store.Upsert(ctx, sk); err != nil {
					rt.Fatalf("edit %s: %v", slug, err)
				}
				edited[slug] = sk.Body
			}
		}

		v2 := map[string]string{}
		for _, slug := range rapid.SliceOfN(slugs, 0, 5).Draw(rt, "v2") {
			v2[slug] = body.Draw(rt, "v2Body")
		}
		reconcileMap(v2)

		bundledNow := map[string]string{}
		for _, sk := range mustList(rt, store, state.BundledScope) {
			bundledNow[sk.Slug] = sk.Body
		}
		ownNow := map[string]string{}
		for _, sk := range mustList(rt, store, state.Scope{}) {
			ownNow[sk.Slug] = sk.Body
		}

		for slug := range v1 {
			hostBody, wasEdited := edited[slug]
			newBody, stillShipped := v2[slug]
			switch {
			case wasEdited && hostBody != v1[slug] && stillShipped && hostBody != newBody && newBody != v1[slug]:
				if bundledNow[slug] != hostBody {
					rt.Errorf("%s: host edit %q lost, bundled body is %q", slug, hostBody, bundledNow[slug])
				}
			case wasEdited && hostBody != v1[slug] && !stillShipped:
				if ownNow[slug] != hostBody {
					rt.Errorf("%s: host edit %q not released, own scope holds %q", slug, hostBody, ownNow[slug])
				}
			case stillShipped && (!wasEdited || hostBody == v1[slug]):
				if bundledNow[slug] != newBody {
					rt.Errorf("%s: untouched skill holds %q, pack ships %q", slug, bundledNow[slug], newBody)
				}
			}
		}

		if again := reconcileMap(v2); len(again.Added)+len(again.Updated)+len(again.Removed)+len(again.Released)+len(again.Cleared)+len(again.Diverged) != 0 {
			rt.Errorf("a second run with the same pack changed the store: %+v", again)
		}
	})
}

func mustList(rt *rapid.T, store state.SkillStore, scope state.Scope) []state.Skill {
	list, err := store.List(context.Background(), scope)
	if err != nil {
		rt.Fatalf("list %v: %v", scope, err)
	}
	return list
}

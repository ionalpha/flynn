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
			uses := rapid.IntRange(0, 9).Draw(rt, "uses")
			sk := bundledSkill(slug, body.Draw(rt, "seededBody"))
			sk.Uses, sk.Wins = uses, uses
			if _, err := store.Upsert(ctx, sk); err != nil {
				rt.Fatalf("seed %d: %v", i, err)
			}
			seeded[slug] = uses
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
			if sk.Uses != seeded[sk.Slug] || sk.Wins != seeded[sk.Slug] {
				rt.Errorf("%s: uses/wins %d/%d, want %d", sk.Slug, sk.Uses, sk.Wins, seeded[sk.Slug])
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

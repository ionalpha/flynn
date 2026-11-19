package statetest

import (
	"context"
	"slices"
	"testing"

	"github.com/ionalpha/flynn/state"
)

// PackOutcome is what a reconciliation reports. Its fields match the engine's result
// type one for one, so a caller converts that result directly; state may not import
// the engine, which is why the suite takes the reconciler as a parameter.
type PackOutcome struct {
	Added, Updated, Unchanged, Removed, Diverged, Released, Adopted, Cleared []string
}

// PackReconciler is the pack-update operation under test: make the bundled scope
// hold want.
type PackReconciler func(ctx context.Context, store state.SkillStore, want []state.Skill) (PackOutcome, error)

// RunPackReconcileSuite runs the pack-update contract through reconcile against
// stores built by newStore: it never overwrites a bundled skill a host edited,
// records that it withheld the update, and clears the record when the stored copy
// and the pack agree again. A host that keeps skills in its own database runs it
// to show its store returns the fields [state.Skill] adds for this, unchanged.
// Each subtest gets a fresh, empty store.
func RunPackReconcileSuite(t *testing.T, newStore func() state.SkillStore, reconcile PackReconciler) {
	t.Helper()
	for _, c := range packReconcileCases {
		t.Run(c.name, func(t *testing.T) { c.run(t, newStore(), reconcile) })
	}
}

var packReconcileCases = []struct {
	name string
	run  func(*testing.T, state.SkillStore, PackReconciler)
}{
	{"UneditedSkillIsUpdated", testPackUneditedUpdated},
	{"EditedSkillKeepsTheEditAndIsDiverged", testPackEditedWithheld},
	{"DivergenceIsReportedOnceAndStays", testPackDivergenceStanding},
	{"EditWithAnUnchangedPackIsNotDiverged", testPackEditPackUnchanged},
	{"DivergenceClearsWhenTheCopiesAgree", testPackClearsOnAgreement},
	{"RevertedEditTakesTheNextUpdate", testPackRevertThenUpdate},
	{"HostOwnedFieldsSurviveAnUpdate", testPackCountersSurvive},
	{"SteadyStateWritesNothing", testPackSteadyState},
	{"RetiredUntouchedSkillIsDeleted", testPackRetiredDeleted},
	{"RetiredEditedSkillIsReleased", testPackRetiredReleased},
	{"ReleaseDoesNotOverwriteASameNamedSkill", testPackReleaseCollision},
	{"SkillSeededWithoutARecordIsAdopted", testPackLegacyAdopted},
}

// packSkill is a skill as the pack ships it.
func packSkill(slug, body string) state.Skill {
	return state.Skill{
		Slug:        slug,
		Name:        "Name of " + slug,
		Description: "When to reach for " + slug + ".",
		Body:        body,
		Tags:        []string{"alpha", "beta"},
		Scope:       state.BundledScope,
	}
}

func reconcileOK(t *testing.T, reconcile PackReconciler, store state.SkillStore, want ...state.Skill) PackOutcome {
	t.Helper()
	res, err := reconcile(context.Background(), store, want)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	return res
}

func getBundled(t *testing.T, store state.SkillStore, slug string) state.Skill {
	t.Helper()
	list, err := store.List(context.Background(), state.BundledScope)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, sk := range list {
		if sk.Slug == slug {
			return sk
		}
	}
	t.Fatalf("bundled skill %q is not in the store", slug)
	return state.Skill{}
}

// hostEdit rewrites a seeded skill the way a host does: read it, change a field,
// write it back.
func hostEdit(t *testing.T, store state.SkillStore, slug string, change func(*state.Skill)) {
	t.Helper()
	sk := getBundled(t, store, slug)
	change(&sk)
	if _, err := store.Upsert(context.Background(), sk); err != nil {
		t.Fatalf("host edit of %q: %v", slug, err)
	}
}

func wantSlugs(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	if want == nil {
		want = []string{}
	}
	if got == nil {
		got = []string{}
	}
	if !slices.Equal(got, want) {
		t.Errorf("%s = %q, want %q", what, got, want)
	}
}

func wantBody(t *testing.T, store state.SkillStore, slug, want string) {
	t.Helper()
	if got := getBundled(t, store, slug).Body; got != want {
		t.Errorf("body of %q = %q, want %q", slug, got, want)
	}
}

func testPackUneditedUpdated(t *testing.T, store state.SkillStore, reconcile PackReconciler) {
	res := reconcileOK(t, reconcile, store, packSkill("a", "v1"))
	wantSlugs(t, "first Added", res.Added, "a")
	res = reconcileOK(t, reconcile, store, packSkill("a", "v2"))
	wantSlugs(t, "Updated", res.Updated, "a")
	wantSlugs(t, "Diverged", res.Diverged)
	wantBody(t, store, "a", "v2")
	if getBundled(t, store, "a").Diverged {
		t.Error("an untouched skill is marked diverged after an update")
	}
}

func testPackEditedWithheld(t *testing.T, store state.SkillStore, reconcile PackReconciler) {
	reconcileOK(t, reconcile, store, packSkill("a", "v1"))
	hostEdit(t, store, "a", func(sk *state.Skill) { sk.Body = "host edit" })

	res := reconcileOK(t, reconcile, store, packSkill("a", "v2"))

	wantSlugs(t, "Diverged", res.Diverged, "a")
	wantSlugs(t, "Updated", res.Updated)
	wantBody(t, store, "a", "host edit")
	if !getBundled(t, store, "a").Diverged {
		t.Error("the withheld skill is not recorded as diverged")
	}
}

func testPackDivergenceStanding(t *testing.T, store state.SkillStore, reconcile PackReconciler) {
	reconcileOK(t, reconcile, store, packSkill("a", "v1"))
	hostEdit(t, store, "a", func(sk *state.Skill) { sk.Body = "host edit" })
	reconcileOK(t, reconcile, store, packSkill("a", "v2"))

	res := reconcileOK(t, reconcile, store, packSkill("a", "v2"))

	wantSlugs(t, "Diverged on the second run", res.Diverged)
	wantSlugs(t, "Updated on the second run", res.Updated)
	wantBody(t, store, "a", "host edit")
	if !getBundled(t, store, "a").Diverged {
		t.Error("the divergence record was lost on a later run")
	}
}

func testPackEditPackUnchanged(t *testing.T, store state.SkillStore, reconcile PackReconciler) {
	reconcileOK(t, reconcile, store, packSkill("a", "v1"))
	hostEdit(t, store, "a", func(sk *state.Skill) { sk.Body = "host edit" })

	res := reconcileOK(t, reconcile, store, packSkill("a", "v1"))

	wantSlugs(t, "Diverged", res.Diverged)
	wantSlugs(t, "Unchanged", res.Unchanged, "a")
	wantBody(t, store, "a", "host edit")
	if getBundled(t, store, "a").Diverged {
		t.Error("a skill the pack did not change is marked diverged")
	}
}

func testPackClearsOnAgreement(t *testing.T, store state.SkillStore, reconcile PackReconciler) {
	reconcileOK(t, reconcile, store, packSkill("a", "v1"))
	hostEdit(t, store, "a", func(sk *state.Skill) { sk.Body = "host edit" })
	reconcileOK(t, reconcile, store, packSkill("a", "v2"))

	hostEdit(t, store, "a", func(sk *state.Skill) { sk.Body = "v2" })
	res := reconcileOK(t, reconcile, store, packSkill("a", "v2"))

	wantSlugs(t, "Cleared", res.Cleared, "a")
	wantSlugs(t, "Diverged", res.Diverged)
	if getBundled(t, store, "a").Diverged {
		t.Error("the divergence record stayed after the copies agreed")
	}
	// The record now holds v2, so the next pack version is an ordinary update.
	res = reconcileOK(t, reconcile, store, packSkill("a", "v3"))
	wantSlugs(t, "Updated after agreement", res.Updated, "a")
	wantBody(t, store, "a", "v3")
}

func testPackRevertThenUpdate(t *testing.T, store state.SkillStore, reconcile PackReconciler) {
	reconcileOK(t, reconcile, store, packSkill("a", "v1"))
	hostEdit(t, store, "a", func(sk *state.Skill) { sk.Body = "host edit" })
	reconcileOK(t, reconcile, store, packSkill("a", "v2"))

	hostEdit(t, store, "a", func(sk *state.Skill) { sk.Body = "v1" })
	res := reconcileOK(t, reconcile, store, packSkill("a", "v2"))

	wantSlugs(t, "Updated", res.Updated, "a")
	wantBody(t, store, "a", "v2")
	if getBundled(t, store, "a").Diverged {
		t.Error("the divergence record stayed after an update applied")
	}
}

func testPackCountersSurvive(t *testing.T, store state.SkillStore, reconcile PackReconciler) {
	reconcileOK(t, reconcile, store, packSkill("a", "v1"))
	hostEdit(t, store, "a", func(sk *state.Skill) { sk.Offers, sk.Reads, sk.Wins = 7, 5, 3 })

	// Counters are the host's, so changing them is not an edit.
	res := reconcileOK(t, reconcile, store, packSkill("a", "v2"))

	wantSlugs(t, "Updated", res.Updated, "a")
	got := getBundled(t, store, "a")
	if got.Body != "v2" || got.Offers != 7 || got.Reads != 5 || got.Wins != 3 {
		t.Errorf("after update body=%q offers=%d reads=%d wins=%d, want v2 7 5 3", got.Body, got.Offers, got.Reads, got.Wins)
	}
}

func testPackSteadyState(t *testing.T, store state.SkillStore, reconcile PackReconciler) {
	reconcileOK(t, reconcile, store, packSkill("a", "v1"))
	before := getBundled(t, store, "a")

	res := reconcileOK(t, reconcile, store, packSkill("a", "v1"))

	wantSlugs(t, "Unchanged", res.Unchanged, "a")
	if n := len(res.Added) + len(res.Updated) + len(res.Removed) + len(res.Diverged) + len(res.Released) + len(res.Cleared); n != 0 {
		t.Errorf("a steady-state run reported changes: %+v", res)
	}
	if after := getBundled(t, store, "a"); after.Version != before.Version {
		t.Errorf("version went %d to %d on a steady-state run", before.Version, after.Version)
	}
}

func testPackRetiredDeleted(t *testing.T, store state.SkillStore, reconcile PackReconciler) {
	reconcileOK(t, reconcile, store, packSkill("a", "v1"), packSkill("b", "v1"))

	res := reconcileOK(t, reconcile, store, packSkill("a", "v1"))

	wantSlugs(t, "Removed", res.Removed, "b")
	wantSlugs(t, "Released", res.Released)
	if _, err := store.Get(context.Background(), "b"); err == nil {
		t.Error("a retired, untouched skill is still readable")
	}
}

func testPackRetiredReleased(t *testing.T, store state.SkillStore, reconcile PackReconciler) {
	ctx := context.Background()
	reconcileOK(t, reconcile, store, packSkill("a", "v1"), packSkill("b", "v1"))
	hostEdit(t, store, "b", func(sk *state.Skill) { sk.Body = "host edit"; sk.Reads = 4 })

	res := reconcileOK(t, reconcile, store, packSkill("a", "v1"))

	wantSlugs(t, "Released", res.Released, "b")
	wantSlugs(t, "Removed", res.Removed)
	list, err := store.List(ctx, state.BundledScope)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Slug != "a" {
		t.Errorf("bundled scope after release holds %d skills, want only a", len(list))
	}
	own, err := store.List(ctx, state.Scope{})
	if err != nil {
		t.Fatal(err)
	}
	if len(own) != 1 || own[0].Slug != "b" || own[0].Body != "host edit" || own[0].Reads != 4 {
		t.Fatalf("released skill in the host's own scope = %+v, want slug b, body %q, reads 4", own, "host edit")
	}
	if own[0].PackDigest != "" || own[0].Diverged {
		t.Errorf("released skill still claims the pack: digest %q diverged %v", own[0].PackDigest, own[0].Diverged)
	}
}

func testPackReleaseCollision(t *testing.T, store state.SkillStore, reconcile PackReconciler) {
	ctx := context.Background()
	reconcileOK(t, reconcile, store, packSkill("b", "v1"))
	hostEdit(t, store, "b", func(sk *state.Skill) { sk.Body = "host edit" })
	if _, err := store.Upsert(ctx, state.Skill{Slug: "b", Body: "the user's own b"}); err != nil {
		t.Fatal(err)
	}

	res := reconcileOK(t, reconcile, store)

	wantSlugs(t, "Released", res.Released)
	wantSlugs(t, "Diverged", res.Diverged, "b")
	wantBody(t, store, "b", "host edit")
	own, err := store.List(ctx, state.Scope{})
	if err != nil {
		t.Fatal(err)
	}
	if len(own) != 1 || own[0].Body != "the user's own b" {
		t.Errorf("the user's own b was overwritten: %+v", own)
	}
}

func testPackLegacyAdopted(t *testing.T, store state.SkillStore, reconcile PackReconciler) {
	ctx := context.Background()
	// Seeded before records existed: stored with no record of what the pack wrote.
	for _, sk := range []state.Skill{packSkill("same", "v2"), packSkill("old", "v1")} {
		if _, err := store.Upsert(ctx, sk); err != nil {
			t.Fatal(err)
		}
	}

	res := reconcileOK(t, reconcile, store, packSkill("same", "v2"), packSkill("old", "v2"))

	wantSlugs(t, "Adopted", res.Adopted, "old", "same")
	wantSlugs(t, "Updated", res.Updated, "old")
	wantSlugs(t, "Diverged", res.Diverged)
	wantBody(t, store, "same", "v2")
	wantBody(t, store, "old", "v2")

	// From here on the records are held, so an edit is protected.
	hostEdit(t, store, "old", func(sk *state.Skill) { sk.Body = "host edit" })
	res = reconcileOK(t, reconcile, store, packSkill("same", "v2"), packSkill("old", "v3"))
	wantSlugs(t, "Diverged after adoption", res.Diverged, "old")
	wantSlugs(t, "Adopted after adoption", res.Adopted)
	wantBody(t, store, "old", "host edit")
}

package bundled

import (
	"context"
	"errors"
	"testing"

	"github.com/ionalpha/flynn/state"
)

// armedStore fails Upsert and Delete on demand, so a test can stop a run between two
// of its writes or make one lose a race.
type armedStore struct {
	state.SkillStore
	upsertErr error
	deleteErr error
}

func (a *armedStore) Upsert(ctx context.Context, sk state.Skill) (state.Skill, error) {
	if a.upsertErr != nil {
		return state.Skill{}, a.upsertErr
	}
	return a.SkillStore.Upsert(ctx, sk)
}

func (a *armedStore) Delete(ctx context.Context, id string) error {
	if a.deleteErr != nil {
		return a.deleteErr
	}
	return a.SkillStore.Delete(ctx, id)
}

func editSkill(t *testing.T, store state.SkillStore, slug, body string) {
	t.Helper()
	sk, err := store.Get(context.Background(), slug)
	if err != nil {
		t.Fatalf("get %s: %v", slug, err)
	}
	sk.Body = body
	if _, err := store.Upsert(context.Background(), sk); err != nil {
		t.Fatalf("edit %s: %v", slug, err)
	}
}

// A write that loses a race may have lost it to a host's edit, so the run must not
// report the skill as updated, and the next run decides again.
func TestALostRaceIsSkippedAndDecidedAgain(t *testing.T) {
	ctx := context.Background()
	store := &armedStore{SkillStore: newStore(t)}
	if _, err := reconcile(ctx, store, []state.Skill{bundledSkill("a", "v1")}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	store.upsertErr = state.ErrConflict
	res, err := reconcile(ctx, store, []state.Skill{bundledSkill("a", "v2")})
	if err != nil {
		t.Fatalf("a lost race ended the run: %v", err)
	}
	if res.Changed() {
		t.Errorf("a write that lost its race was reported: %+v", res)
	}

	store.upsertErr = nil
	res, err = reconcile(ctx, store, []state.Skill{bundledSkill("a", "v2")})
	if err != nil || !equalStrings(res.Updated, []string{"a"}) {
		t.Errorf("the next run reported %+v (err %v), want a updated", res, err)
	}
}

func TestAWriteFailureWhilePlacingEndsTheRun(t *testing.T) {
	ctx := context.Background()
	store := &armedStore{SkillStore: newStore(t)}
	if _, err := reconcile(ctx, store, []state.Skill{bundledSkill("a", "v1")}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	store.upsertErr = errStore
	if _, err := reconcile(ctx, store, []state.Skill{bundledSkill("a", "v2")}); !errors.Is(err, errStore) {
		t.Fatalf("error %v, want the store's own", err)
	}
}

// A release writes the host's copy and then deletes the bundled one. A stop between
// the two leaves both; the next run must finish the delete without a second copy.
func TestAnInterruptedReleaseFinishesOnTheNextRun(t *testing.T) {
	ctx := context.Background()
	store := &armedStore{SkillStore: newStore(t)}
	if _, err := reconcile(ctx, store, []state.Skill{bundledSkill("a", "v1")}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	editSkill(t, store, "a", "the host's wording")

	store.deleteErr = errStore
	if _, err := reconcile(ctx, store, nil); !errors.Is(err, errStore) {
		t.Fatalf("error %v, want the store's own", err)
	}
	store.deleteErr = nil
	res, err := reconcile(ctx, store, nil)
	if err != nil || !equalStrings(res.Released, []string{"a"}) {
		t.Fatalf("the next run reported %+v (err %v), want a released", res, err)
	}
	if got := slugsIn(t, store, state.Scope{}); !equalStrings(got, []string{"a"}) {
		t.Errorf("host scope holds %v, want one copy of a", got)
	}
	if got := slugsIn(t, store, state.BundledScope); len(got) != 0 {
		t.Errorf("bundled scope still holds %v", got)
	}
}

// A dropped, edited skill whose slug the host already uses is kept and marked once;
// the mark is not reported again on later runs.
func TestAReleaseCollisionIsReportedOnce(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	if _, err := reconcile(ctx, store, []state.Skill{bundledSkill("a", "v1")}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	editSkill(t, store, "a", "the host's wording")
	if _, err := store.Upsert(ctx, state.Skill{Slug: "a", Name: "mine", Body: "somebody else's"}); err != nil {
		t.Fatalf("own skill: %v", err)
	}

	first, err := reconcile(ctx, store, nil)
	if err != nil || !equalStrings(first.Diverged, []string{"a"}) || len(first.Released) != 0 {
		t.Fatalf("first run reported %+v (err %v), want a diverged and nothing released", first, err)
	}
	second, err := reconcile(ctx, store, nil)
	if err != nil || second.Changed() {
		t.Fatalf("second run reported %+v (err %v), want it silent", second, err)
	}
	if got := slugsIn(t, store, state.BundledScope); !equalStrings(got, []string{"a"}) {
		t.Errorf("bundled scope holds %v, want the edited copy kept", got)
	}
}

// A host that reads the pack's version back into its edit has resolved the
// divergence, with the pack unchanged since: the mark clears and nothing is lost.
func TestDivergenceClearsWhenTheHostAdoptsThePacksVersion(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	if _, err := reconcile(ctx, store, []state.Skill{bundledSkill("a", "v1")}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	editSkill(t, store, "a", "the host's wording")
	if _, err := reconcile(ctx, store, []state.Skill{bundledSkill("a", "v2")}); err != nil {
		t.Fatalf("update: %v", err)
	}
	editSkill(t, store, "a", "v2")

	res, err := reconcile(ctx, store, []state.Skill{bundledSkill("a", "v2")})
	if err != nil || !equalStrings(res.Cleared, []string{"a"}) {
		t.Fatalf("reported %+v (err %v), want a cleared", res, err)
	}
	if got, _ := store.Get(ctx, "a"); got.Diverged {
		t.Error("a is still marked diverged")
	}
}

func TestReleaseFailuresAreReported(t *testing.T) {
	ctx := context.Background()
	store := &armedStore{SkillStore: newStore(t)}
	if _, err := reconcile(ctx, store, []state.Skill{bundledSkill("a", "v1")}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	editSkill(t, store, "a", "the host's wording")
	store.upsertErr = errStore
	if _, err := reconcile(ctx, store, nil); !errors.Is(err, errStore) {
		t.Fatalf("error %v, want the store's own", err)
	}
}

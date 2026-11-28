package bundled

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

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
	if _, err := reconcile(ctx, store, []state.Skill{bundledSkill("a", "v1")}, nil); err != nil {
		t.Fatalf("seed: %v", err)
	}

	store.upsertErr = state.ErrConflict
	res, err := reconcile(ctx, store, []state.Skill{bundledSkill("a", "v2")}, nil)
	if err != nil {
		t.Fatalf("a lost race ended the run: %v", err)
	}
	if res.Changed() {
		t.Errorf("a write that lost its race was reported: %+v", res)
	}

	store.upsertErr = nil
	res, err = reconcile(ctx, store, []state.Skill{bundledSkill("a", "v2")}, nil)
	if err != nil || !equalStrings(res.Updated, []string{"a"}) {
		t.Errorf("the next run reported %+v (err %v), want a updated", res, err)
	}
}

func TestAWriteFailureWhilePlacingEndsTheRun(t *testing.T) {
	ctx := context.Background()
	store := &armedStore{SkillStore: newStore(t)}
	if _, err := reconcile(ctx, store, []state.Skill{bundledSkill("a", "v1")}, nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	store.upsertErr = errStore
	if _, err := reconcile(ctx, store, []state.Skill{bundledSkill("a", "v2")}, nil); !errors.Is(err, errStore) {
		t.Fatalf("error %v, want the store's own", err)
	}
}

// A release writes the host's copy and then deletes the bundled one. A stop between
// the two leaves both; the next run must finish the delete without a second copy.
func TestAnInterruptedReleaseFinishesOnTheNextRun(t *testing.T) {
	ctx := context.Background()
	store := &armedStore{SkillStore: newStore(t)}
	if _, err := reconcile(ctx, store, []state.Skill{bundledSkill("a", "v1")}, nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	editSkill(t, store, "a", "the host's wording")

	store.deleteErr = errStore
	if _, err := reconcile(ctx, store, nil, nil); !errors.Is(err, errStore) {
		t.Fatalf("error %v, want the store's own", err)
	}
	store.deleteErr = nil
	res, err := reconcile(ctx, store, nil, nil)
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
	if _, err := reconcile(ctx, store, []state.Skill{bundledSkill("a", "v1")}, nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	editSkill(t, store, "a", "the host's wording")
	if _, err := store.Upsert(ctx, state.Skill{Slug: "a", Name: "mine", Body: "somebody else's"}); err != nil {
		t.Fatalf("own skill: %v", err)
	}

	first, err := reconcile(ctx, store, nil, nil)
	if err != nil || !equalStrings(first.Diverged, []string{"a"}) || len(first.Released) != 0 {
		t.Fatalf("first run reported %+v (err %v), want a diverged and nothing released", first, err)
	}
	second, err := reconcile(ctx, store, nil, nil)
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
	if _, err := reconcile(ctx, store, []state.Skill{bundledSkill("a", "v1")}, nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	editSkill(t, store, "a", "the host's wording")
	if _, err := reconcile(ctx, store, []state.Skill{bundledSkill("a", "v2")}, nil); err != nil {
		t.Fatalf("update: %v", err)
	}
	editSkill(t, store, "a", "v2")

	res, err := reconcile(ctx, store, []state.Skill{bundledSkill("a", "v2")}, nil)
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
	if _, err := reconcile(ctx, store, []state.Skill{bundledSkill("a", "v1")}, nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	editSkill(t, store, "a", "the host's wording")
	store.upsertErr = errStore
	if _, err := reconcile(ctx, store, nil, nil); !errors.Is(err, errStore) {
		t.Fatalf("error %v, want the store's own", err)
	}
}

// TestDigestCoversExactlyThePackOwnedFields classifies every field of [state.Skill] as
// owned by the pack or by the host and checks the digest agrees. A field added to the
// struct fails here until someone decides which side it is on: left unclassified, a
// pack-owned field would never register as an edit, and a host-owned one would read as
// an edit on every run.
func TestDigestCoversExactlyThePackOwnedFields(t *testing.T) {
	packOwned := map[string]bool{"Slug": true, "Name": true, "Description": true, "Body": true, "Check": true, "Tags": true}
	hostOwned := map[string]bool{
		"ID": true, "Scope": true, "Offers": true, "Reads": true, "Wins": true,
		"PackDigest": true, "Diverged": true, "Version": true, "CreatedAt": true,
		"UpdatedAt": true, "Envelope": true,
	}
	typ := reflect.TypeOf(state.Skill{})
	base := state.Skill{Slug: "s", Name: "n", Description: "d", Body: "b", Check: "c", Tags: []string{"t"}}
	want := digest(base)
	for i := range typ.NumField() {
		name := typ.Field(i).Name
		if packOwned[name] == hostOwned[name] {
			t.Errorf("state.Skill.%s is not classified as exactly one of pack-owned or host-owned in this test", name)
			continue
		}
		changed := base
		field := reflect.ValueOf(&changed).Elem().Field(i)
		fillNonZero(field)
		if field.IsZero() {
			t.Errorf("state.Skill.%s: the test could not set a value, extend fillNonZero", name)
			continue
		}
		got := digest(changed) != want
		if got != packOwned[name] {
			t.Errorf("state.Skill.%s changes the digest = %v, but it is classified pack-owned = %v", name, got, packOwned[name])
		}
	}
}

// fillNonZero sets v, or the first settable leaf inside it, to a value that differs
// from the one it holds.
func fillNonZero(v reflect.Value) {
	switch v.Kind() {
	case reflect.String:
		v.SetString(v.String() + "x")
	case reflect.Int, reflect.Int64:
		v.SetInt(v.Int() + 7)
	case reflect.Bool:
		v.SetBool(!v.Bool())
	case reflect.Slice:
		v.Set(reflect.Append(v, reflect.New(v.Type().Elem()).Elem()))
		if v.Type().Elem().Kind() == reflect.String {
			v.Index(v.Len() - 1).SetString("extra")
		}
	case reflect.Struct:
		if v.Type() == reflect.TypeOf(time.Time{}) {
			v.Set(reflect.ValueOf(time.Unix(1, 0)))
			return
		}
		for i := range v.NumField() {
			if v.Field(i).CanSet() {
				fillNonZero(v.Field(i))
				if !v.IsZero() {
					return
				}
			}
		}
	}
}

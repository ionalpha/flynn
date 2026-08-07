package learn

import (
	"context"
	"errors"
	"testing"

	"github.com/ionalpha/flynn/state"
)

// seedBundled writes a skill into the reserved scope the way the pack seeder will,
// so the tests below exercise the boundary against a real record rather than a
// flag.
func seedBundled(t *testing.T, skills state.SkillStore, slug, body string) state.Skill {
	t.Helper()
	sk, err := skills.Upsert(context.Background(), state.Skill{
		Slug:        slug,
		Name:        "Systematic Debugging",
		Description: "How to find a fault by bisection rather than by guessing.",
		Body:        body,
		Check:       "true",
		Tags:        []string{"craft"},
		Scope:       state.BundledScope,
	})
	if err != nil {
		t.Fatal(err)
	}
	return sk
}

// The bundled scope holds skills shipped in the binary, so the capture path must
// refuse it as a destination outright: distilling into it would mix learned output
// with authored content that an upgrade replaces wholesale.
func TestCurateRefusesBundledScope(t *testing.T) {
	skills, memories := newStores(t)
	d := &fakeDistiller{lessons: []Lesson{{Kind: LessonSkill, Title: "Anything", Body: "b"}}}

	o := convergedOutcome()
	o.Scope = state.BundledScope
	captured, err := NewCurator(d, skills, memories).Curate(context.Background(), o)
	if !errors.Is(err, ErrBundledScope) {
		t.Fatalf("Curate into the bundled scope: err = %v, want ErrBundledScope", err)
	}
	if len(captured.Skills) != 0 || len(captured.Memories) != 0 {
		t.Fatalf("refused capture still stored something: %+v", captured)
	}
	if d.called != 0 {
		t.Fatalf("distiller called %d times; a refused scope must not spend a distillation", d.called)
	}
}

// A bundled skill owns its slug in every scope, not just its own. Without this the
// loop would store a learned skill that no read can reach, because Get resolves a
// slug across scopes and the bundled record is always the older one.
func TestCurateSkipsSlugOwnedByBundledSkill(t *testing.T) {
	skills, memories := newStores(t)
	ctx := context.Background()
	bundled := seedBundled(t, skills, "systematic-debugging", "The authored procedure.")

	d := &fakeDistiller{lessons: []Lesson{
		{Kind: LessonSkill, Title: "Systematic Debugging", Body: "A learned procedure."},
	}}
	captured, err := NewCurator(d, skills, memories).Curate(ctx, convergedOutcome())
	if err != nil {
		t.Fatal(err)
	}
	if len(captured.Skills) != 0 {
		t.Fatalf("captured %d skills; a bundled slug must store nothing", len(captured.Skills))
	}
	if len(captured.Skipped) != 1 {
		t.Fatalf("Skipped = %d, want 1 so the caller can report the collision", len(captured.Skipped))
	}

	// Nothing was written into the run's own scope either: a shadowed record is as
	// bad as an overwritten one, because it counts as stored and can never be read.
	learned, err := skills.List(ctx, convergedOutcome().Scope)
	if err != nil {
		t.Fatal(err)
	}
	if len(learned) != 0 {
		t.Fatalf("run scope holds %d skills, want 0: %+v", len(learned), learned)
	}

	got, err := skills.Get(ctx, "systematic-debugging")
	if err != nil {
		t.Fatal(err)
	}
	if got.Body != bundled.Body || got.Version != bundled.Version {
		t.Fatalf("bundled skill was modified: %+v", got)
	}
}

// A lesson that collides with nothing bundled still lands, so the guard is a
// boundary rather than a blanket refusal.
func TestCurateStoresWhenNoBundledCollision(t *testing.T) {
	skills, memories := newStores(t)
	ctx := context.Background()
	seedBundled(t, skills, "systematic-debugging", "The authored procedure.")

	d := &fakeDistiller{lessons: []Lesson{
		{Kind: LessonSkill, Title: "Reset the Widget", Body: "A learned procedure."},
	}}
	captured, err := NewCurator(d, skills, memories).Curate(ctx, convergedOutcome())
	if err != nil {
		t.Fatal(err)
	}
	if len(captured.Skills) != 1 {
		t.Fatalf("captured %d skills, want 1: %+v", len(captured.Skills), captured)
	}
	if captured.Skills[0].Scope != convergedOutcome().Scope {
		t.Fatalf("learned skill stored at %+v, want the run's scope", captured.Skills[0].Scope)
	}
}

// failingBundledList is a skill store whose read of the bundled scope fails,
// standing in for a store that is unreachable at the moment a run is distilled.
type failingBundledList struct {
	state.SkillStore
	err error
}

func (f failingBundledList) List(ctx context.Context, scope state.Scope) ([]state.Skill, error) {
	if scope == state.BundledScope {
		return nil, f.err
	}
	return f.SkillStore.List(ctx, scope)
}

// If the bundled set cannot be read, the loop does not know which slugs are
// spoken for, so it stores nothing rather than capturing on the assumption that
// none are.
func TestCurateAbortsWhenBundledSetIsUnreadable(t *testing.T) {
	skills, memories := newStores(t)
	boom := errors.New("store unavailable")
	d := &fakeDistiller{lessons: []Lesson{
		{Kind: LessonSkill, Title: "Reset the Widget", Body: "A learned procedure."},
	}}

	captured, err := NewCurator(d, failingBundledList{skills, boom}, memories).
		Curate(context.Background(), convergedOutcome())
	if !errors.Is(err, boom) {
		t.Fatalf("Curate: err = %v, want the store's error", err)
	}
	if len(captured.Skills) != 0 {
		t.Fatalf("stored %d skills without knowing the bundled slugs: %+v", len(captured.Skills), captured.Skills)
	}
}

// Decay retires a skill by policy and Regrade rewrites its tags. Both are wrong
// for content that ships with the binary and is replaced by an upgrade, so both
// refuse the scope rather than leaving it to the caller to remember.
func TestDecayAndRegradeRefuseBundledScope(t *testing.T) {
	skills, _ := newStores(t)
	ctx := context.Background()
	seedBundled(t, skills, "systematic-debugging", "The authored procedure.")

	if _, err := Decay(ctx, skills, state.BundledScope, DefaultDecay()); !errors.Is(err, ErrBundledScope) {
		t.Fatalf("Decay on the bundled scope: err = %v, want ErrBundledScope", err)
	}
	if _, err := Regrade(ctx, skills, state.BundledScope, fakeVerifier{}); !errors.Is(err, ErrBundledScope) {
		t.Fatalf("Regrade on the bundled scope: err = %v, want ErrBundledScope", err)
	}
}

// Recall is scope-blind, so the same slug can be held by a bundled skill and a
// learned one at once. Reinforcement is credited by id for that reason: by slug it
// would land on whichever record the store happens to resolve to.
func TestReinforceCreditsTheRecalledRecord(t *testing.T) {
	skills, _ := newStores(t)
	ctx := context.Background()
	bundled := seedBundled(t, skills, "shared-slug", "The bundled procedure.")

	learned, err := skills.Upsert(ctx, state.Skill{
		Slug:  "shared-slug",
		Name:  "Shared Slug",
		Body:  "The learned procedure.",
		Tags:  []string{provenanceTag},
		Scope: state.Scope{Instance: "inst"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := Reinforce(ctx, skills, []string{learned.ID}, true); err != nil {
		t.Fatal(err)
	}

	got, err := skills.Get(ctx, learned.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Uses != 1 || got.Wins != 1 {
		t.Fatalf("recalled skill uses/wins = %d/%d, want 1/1", got.Uses, got.Wins)
	}
	other, err := skills.Get(ctx, bundled.ID)
	if err != nil {
		t.Fatal(err)
	}
	if other.Uses != 0 || other.Wins != 0 {
		t.Fatalf("bundled skill uses/wins = %d/%d, want 0/0: evidence was credited to the wrong record", other.Uses, other.Wins)
	}
}

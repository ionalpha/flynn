package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/skill/bundled"
	"github.com/ionalpha/flynn/state"
)

// plantBundledSkill writes a skill into the reserved scope the way a previous
// version of the binary would have seeded it, so a test can watch what this one does
// with a set it did not ship.
func plantBundledSkill(ctx context.Context, t *testing.T, skills state.SkillStore, slug string) {
	t.Helper()
	if _, err := skills.Upsert(ctx, state.Skill{
		Slug:        slug,
		Name:        slug,
		Description: "Shipped by an earlier version.",
		Body:        "the old body",
		Scope:       state.BundledScope,
	}); err != nil {
		t.Fatalf("plant %s: %v", slug, err)
	}
}

func bundledSlugs(ctx context.Context, t *testing.T, skills state.SkillStore) []string {
	t.Helper()
	list, err := skills.List(ctx, state.BundledScope)
	if err != nil {
		t.Fatalf("list bundled: %v", err)
	}
	out := make([]string, 0, len(list))
	for _, sk := range list {
		out = append(out, sk.Slug)
	}
	return out
}

// Opening the store is what reconciles the pack, so the guarantee under test is that
// after any command opens it, the reserved scope holds exactly what this binary
// ships: a skill an older version seeded and this one dropped is gone.
func TestOpeningTheStoreReconcilesTheBundledPack(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	store, err := openDataStore(ctx, dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	plantBundledSkill(ctx, t, store.Skills(), "shipped-by-an-older-build")
	if _, err := store.Skills().Upsert(ctx, state.Skill{Slug: "mine", Name: "mine", Body: "my own"}); err != nil {
		t.Fatalf("upsert user skill: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	store, err = openDataStore(ctx, dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = store.Close() }()

	want, err := bundled.Skills()
	if err != nil {
		t.Fatalf("read pack: %v", err)
	}
	got := bundledSlugs(ctx, t, store.Skills())
	if len(got) != len(want) {
		t.Fatalf("bundled scope holds %v, want the %d skill(s) this binary ships", got, len(want))
	}
	for i, sk := range want {
		if got[i] != sk.Slug {
			t.Errorf("bundled scope holds %q at %d, want %q", got[i], i, sk.Slug)
		}
	}
	if _, err := store.Skills().Get(ctx, "mine"); err != nil {
		t.Errorf("the user's own skill did not survive reconciliation: %v", err)
	}
}

// The opt-out has to leave the store without the bundled set, not merely without
// this start's additions to it, or a run measured against a clean baseline is still
// recalling skills an earlier start seeded.
func TestOptOutRemovesTheBundledSet(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	store, err := openDataStore(ctx, dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	plantBundledSkill(ctx, t, store.Skills(), "alpha")
	plantBundledSkill(ctx, t, store.Skills(), "beta")
	if _, err := store.Skills().Upsert(ctx, state.Skill{Slug: "mine", Name: "mine", Body: "my own"}); err != nil {
		t.Fatalf("upsert user skill: %v", err)
	}

	t.Cleanup(func() { bundledSkillsDisabled = false })
	bundledSkillsDisabled = true
	if err := reconcileBundledSkills(ctx, store); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	defer func() { _ = store.Close() }()

	if got := bundledSlugs(ctx, t, store.Skills()); len(got) != 0 {
		t.Errorf("bundled scope still holds %v", got)
	}
	if _, err := store.Skills().Get(ctx, "mine"); err != nil {
		t.Errorf("the opt-out took the user's own skill with it: %v", err)
	}
}

// The flag has to reach the store, which is the one thing a flag defined in one file
// and read in another can silently fail to do.
func TestNoBundledSkillsFlagIsWired(t *testing.T) {
	t.Cleanup(func() { bundledSkillsDisabled = false })
	fs := flag.NewFlagSet("flynn", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if code := run(fs, []string{"-no-bundled-skills", "-version"}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("run exited %d", code)
	}
	if !bundledSkillsDisabled {
		t.Error("--no-bundled-skills did not disable the bundled set")
	}
}

// A bundled skill is listed alongside the learned ones and marked, because it is
// recalled alongside them: a listing that showed only what this install learned
// would omit most of what the agent reaches for.
func TestRenderSkillsMarksBundledOnes(t *testing.T) {
	ctx := context.Background()
	st := memStore(t)
	plantBundledSkill(ctx, t, st.Skills(), "craft")
	if _, err := st.Skills().Upsert(ctx, state.Skill{Slug: "learned-here", Name: "learned here", Body: "from a run"}); err != nil {
		t.Fatalf("upsert learned skill: %v", err)
	}

	var sb bytes.Buffer
	renderSkills(ctx, &sb, st.Skills())
	out := sb.String()
	for _, want := range []string{"2 skill(s), 1 of them learned here", "craft [bundled]", "learned here (used 0"} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "learned here [bundled]") {
		t.Errorf("a learned skill was marked bundled:\n%s", out)
	}
}

// A store that will not answer says so. Either scope can be the one that fails, and
// a listing that reported the other half as the whole would read as "you have one
// skill" when the truth is that it could not tell.
func TestRenderSkillsReportsAReadFailure(t *testing.T) {
	for _, scope := range []state.Scope{state.BundledScope, {}} {
		var sb bytes.Buffer
		renderSkills(context.Background(), &sb, unreadableSkills{fail: scope})
		if !strings.Contains(sb.String(), "could not read skills") {
			t.Errorf("scope %v: render said %q", scope, sb.String())
		}
	}
}

// unreadableSkills refuses to list one scope and answers empty for the rest.
type unreadableSkills struct {
	state.SkillStore
	fail state.Scope
}

func (u unreadableSkills) List(_ context.Context, scope state.Scope) ([]state.Skill, error) {
	if scope == u.fail {
		return nil, errors.New("store unavailable")
	}
	return nil, nil
}

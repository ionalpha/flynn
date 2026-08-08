package bundled

import (
	"context"
	"errors"
	"io/fs"
	"testing"
	"testing/fstest"

	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/skill"
	"github.com/ionalpha/flynn/skill/skillmd"
	"github.com/ionalpha/flynn/state"
)

func newStore(t *testing.T) state.SkillStore {
	t.Helper()
	reg := resource.NewRegistry()
	if err := resource.RegisterCoreKinds(reg); err != nil {
		t.Fatalf("register core kinds: %v", err)
	}
	if err := skill.RegisterKind(reg); err != nil {
		t.Fatalf("register skill kind: %v", err)
	}
	return skill.NewStore(resource.NewMemory(reg))
}

// bundledSkill is a skill as the pack would produce it: already in the reserved
// scope, with the fields a SKILL.md can express.
func bundledSkill(slug, body string) state.Skill {
	return state.Skill{
		Slug:        slug,
		Name:        slug,
		Description: "What " + slug + " is for, and when to reach for it.",
		Body:        body,
		Scope:       state.BundledScope,
	}
}

func slugsIn(t *testing.T, store state.SkillStore, scope state.Scope) []string {
	t.Helper()
	list, err := store.List(context.Background(), scope)
	if err != nil {
		t.Fatalf("list %v: %v", scope, err)
	}
	out := make([]string, 0, len(list))
	for _, sk := range list {
		out = append(out, sk.Slug)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// The pack in the binary is held to the specification it claims to follow, so a
// skill that would fail to load on a user's first start fails in CI instead. It is
// also held to being exportable: a skill we ship has to survive the round trip back
// out to a skill tree, and the field that usually breaks that is the description.
func TestPackConforms(t *testing.T) {
	packs, err := Packs()
	if err != nil {
		t.Fatalf("load pack: %v", err)
	}
	skills, err := Skills()
	if err != nil {
		t.Fatalf("map pack: %v", err)
	}
	if len(skills) != len(packs) {
		t.Fatalf("mapped %d skills from %d packs", len(skills), len(packs))
	}
	seen := map[string]bool{}
	for _, sk := range skills {
		if sk.Scope != state.BundledScope {
			t.Errorf("%s: scope %v, want the bundled scope", sk.Slug, sk.Scope)
		}
		if sk.Description == "" {
			t.Errorf("%s: no description", sk.Slug)
		}
		if seen[sk.Slug] {
			t.Errorf("%s: slug shipped twice", sk.Slug)
		}
		seen[sk.Slug] = true
		doc, err := skill.ToDoc(sk)
		if err != nil {
			t.Errorf("%s: not exportable: %v", sk.Slug, err)
			continue
		}
		if err := skillmd.Validate(doc, doc.Name); err != nil {
			t.Errorf("%s: exported document is not conformant: %v", sk.Slug, err)
		}
	}
}

// The pack has to be in the binary, not merely in the source tree. An embed pattern
// that stops matching is silent: Packs returns nothing and every seed is a no-op, so
// the check is that the filesystem is readable and holds the directory it names.
func TestPackIsEmbedded(t *testing.T) {
	entries, err := fs.ReadDir(FS(), Root)
	if err != nil {
		t.Fatalf("read %s from the embedded pack: %v", Root, err)
	}
	if len(entries) == 0 {
		t.Fatalf("%s is empty in the binary", Root)
	}
}

// A skill tree becomes skills in the reserved scope, carrying the fields the format
// has a home for. The tree here stands in for the pack: the mapping has to be tested
// against skills, and the pack in the binary is not where a test gets to put them.
func TestSkillsFromATree(t *testing.T) {
	tree := fstest.MapFS{
		"pack/deploy/SKILL.md": &fstest.MapFile{Data: []byte(
			"---\nname: deploy\ndescription: Ship a reviewed change to production.\n" +
				"metadata:\n  ionagent.io/title: Deploy a release\n  ionagent.io/tags: '[\"release\",\"ops\"]'\n---\n\nRun the deploy script.\n")},
		"pack/audit/SKILL.md": &fstest.MapFile{Data: []byte(
			"---\nname: audit\ndescription: Read a change for the mistakes review misses.\n---\n\nStart with the diff.\n")},
	}
	got, err := skillsFrom(tree, "pack")
	if err != nil {
		t.Fatalf("map tree: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("mapped %d skills, want 2", len(got))
	}
	if got[0].Slug != "audit" || got[1].Slug != "deploy" {
		t.Fatalf("mapped %q and %q, want them in slug order", got[0].Slug, got[1].Slug)
	}
	deploy := got[1]
	if deploy.Name != "Deploy a release" {
		t.Errorf("name %q, want the title from metadata", deploy.Name)
	}
	if deploy.Description != "Ship a reviewed change to production." {
		t.Errorf("description %q", deploy.Description)
	}
	if !equalStrings(deploy.Tags, []string{"release", "ops"}) {
		t.Errorf("tags %v", deploy.Tags)
	}
	if deploy.Scope != state.BundledScope {
		t.Errorf("scope %v, want the bundled scope", deploy.Scope)
	}
}

// A tree that breaks the format stops the mapping. Seeding half a pack and calling
// it a start would leave an install short of skills with nothing said about it.
// Both halves of the mapping are checked, because they fail in different places: a
// document the codec cannot parse, and a document that parses but carries one of our
// own metadata fields in a shape the store has no home for.
func TestSkillsFromABrokenTreeFails(t *testing.T) {
	for name, doc := range map[string]string{
		"unparseable":  "no frontmatter here\n",
		"bad metadata": "---\nname: deploy\ndescription: Ship a reviewed change.\nmetadata:\n  ionagent.io/tags: release, ops\n---\n\nBody.\n",
	} {
		t.Run(name, func(t *testing.T) {
			tree := fstest.MapFS{"pack/deploy/SKILL.md": &fstest.MapFile{Data: []byte(doc)}}
			if _, err := skillsFrom(tree, "pack"); err == nil {
				t.Fatal("mapped without error")
			}
		})
	}
}

// Seeding the real pack is what a first start does, and doing it twice is what a
// second start does. The second must write nothing: a reconciliation that bumped
// every version on every start would make the store's history a log of restarts.
func TestSeedIsIdempotent(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	first, err := Seed(ctx, store)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	second, err := Seed(ctx, store)
	if err != nil {
		t.Fatalf("reseed: %v", err)
	}
	if second.Changed() {
		t.Errorf("second seed changed the store: %+v", second)
	}
	if len(second.Unchanged) != len(first.Added)+len(first.Unchanged) {
		t.Errorf("second seed saw %d unchanged, first added %d", len(second.Unchanged), len(first.Added))
	}
	if got := len(slugsIn(t, store, state.BundledScope)); got != len(first.Added) {
		t.Errorf("bundled scope holds %d skills, seed added %d", got, len(first.Added))
	}
	if got := slugsIn(t, store, state.Scope{}); len(got) != 0 {
		t.Errorf("seeding wrote %v into the zero scope", got)
	}
}

func TestReconcileAddsUpdatesAndRemoves(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	res, err := reconcile(ctx, store, []state.Skill{bundledSkill("alpha", "one"), bundledSkill("beta", "two")})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if !equalStrings(res.Added, []string{"alpha", "beta"}) {
		t.Fatalf("added %v, want alpha and beta", res.Added)
	}

	// The same pack again writes nothing.
	res, err = reconcile(ctx, store, []state.Skill{bundledSkill("alpha", "one"), bundledSkill("beta", "two")})
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	if res.Changed() || !equalStrings(res.Unchanged, []string{"alpha", "beta"}) {
		t.Fatalf("restart reported %+v, want both unchanged", res)
	}

	// The next version of the binary rewrites one skill and stops shipping the other.
	res, err = reconcile(ctx, store, []state.Skill{bundledSkill("alpha", "one, revised")})
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	if !equalStrings(res.Updated, []string{"alpha"}) || !equalStrings(res.Removed, []string{"beta"}) {
		t.Fatalf("upgrade reported %+v, want alpha updated and beta removed", res)
	}
	if got := slugsIn(t, store, state.BundledScope); !equalStrings(got, []string{"alpha"}) {
		t.Fatalf("bundled scope holds %v, want alpha alone", got)
	}
	got, err := store.Get(ctx, "alpha")
	if err != nil {
		t.Fatalf("get alpha: %v", err)
	}
	if got.Body != "one, revised" {
		t.Errorf("alpha body %q, want the upgraded one", got.Body)
	}
}

// Outcome evidence is about runs on this machine, so an upgrade that replaces a
// skill's body must not reset how well it has performed.
func TestUpgradeKeepsOutcomeEvidence(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	if _, err := reconcile(ctx, store, []state.Skill{bundledSkill("alpha", "one")}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	seeded, err := store.Get(ctx, "alpha")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	seeded.Uses, seeded.Wins = 7, 4
	if _, err := store.Upsert(ctx, seeded); err != nil {
		t.Fatalf("record outcomes: %v", err)
	}

	if _, err := reconcile(ctx, store, []state.Skill{bundledSkill("alpha", "one, revised")}); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	got, err := store.Get(ctx, "alpha")
	if err != nil {
		t.Fatalf("get after upgrade: %v", err)
	}
	if got.Uses != 7 || got.Wins != 4 {
		t.Errorf("uses/wins %d/%d after upgrade, want 7/4", got.Uses, got.Wins)
	}
	if got.Body != "one, revised" {
		t.Errorf("body %q, want the upgraded one", got.Body)
	}
}

// Running without the bundled set has to mean the set is gone, not merely that this
// start did not add to it, or a baseline measured without it still recalls it.
func TestPruneEmptiesTheBundledScope(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	if _, err := reconcile(ctx, store, []state.Skill{bundledSkill("alpha", "one"), bundledSkill("beta", "two")}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	res, err := Prune(ctx, store)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if !equalStrings(res.Removed, []string{"alpha", "beta"}) {
		t.Fatalf("prune removed %v, want both", res.Removed)
	}
	if got := slugsIn(t, store, state.BundledScope); len(got) != 0 {
		t.Fatalf("bundled scope still holds %v", got)
	}
}

// A slug is unique within a scope, not across them, and Delete resolves a bare slug
// across every scope. Removing a bundled skill must therefore take the user's skill
// of the same name with it in no circumstance.
func TestRemovalLeavesASameNamedUserSkillAlone(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	mine, err := store.Upsert(ctx, state.Skill{Slug: "alpha", Name: "my own alpha", Body: "mine"})
	if err != nil {
		t.Fatalf("upsert user skill: %v", err)
	}
	if _, err := reconcile(ctx, store, []state.Skill{bundledSkill("alpha", "one")}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := Prune(ctx, store); err != nil {
		t.Fatalf("prune: %v", err)
	}
	got, err := store.Get(ctx, mine.ID)
	if err != nil {
		t.Fatalf("the user's skill is gone: %v", err)
	}
	if got.Body != "mine" {
		t.Errorf("user skill body %q, want it untouched", got.Body)
	}
	if slugs := slugsIn(t, store, state.BundledScope); len(slugs) != 0 {
		t.Errorf("bundled scope still holds %v", slugs)
	}
}

// A store that will not cooperate is reported rather than worked around. Each of the
// three operations is its own case because each one failing silently would leave the
// scope in a different wrong state: a read treated as empty re-adds what an upgrade
// removed, a failed write leaves the pack half-applied, and a failed delete leaves a
// skill this binary does not ship.
func TestStoreFailuresAreReported(t *testing.T) {
	ctx := context.Background()
	pack := []state.Skill{bundledSkill("alpha", "one")}

	for _, tc := range []struct {
		name  string
		store state.SkillStore
		want  []state.Skill
	}{
		{"read", failingStore{fail: "list"}, pack},
		{"write", failingStore{fail: "upsert"}, pack},
		{"delete", failingStore{fail: "delete", have: pack}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := reconcile(ctx, tc.store, tc.want); !errors.Is(err, errStore) {
				t.Fatalf("error %v, want the store's own", err)
			}
		})
	}
}

var errStore = errors.New("store unavailable")

// failingStore answers one operation with errStore and the rest as an empty store
// would, so a test can put the failure exactly where it wants it.
type failingStore struct {
	state.SkillStore
	fail string
	have []state.Skill
}

func (f failingStore) List(context.Context, state.Scope) ([]state.Skill, error) {
	if f.fail == "list" {
		return nil, errStore
	}
	return f.have, nil
}

func (f failingStore) Upsert(_ context.Context, sk state.Skill) (state.Skill, error) {
	if f.fail == "upsert" {
		return state.Skill{}, errStore
	}
	return sk, nil
}

func (f failingStore) Delete(context.Context, string) error {
	if f.fail == "delete" {
		return errStore
	}
	return nil
}

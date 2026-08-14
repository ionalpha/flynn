// Package bundled carries the authored skill pack inside the binary and reconciles
// it into state.BundledScope, so a fresh install knows the craft before the learning
// loop has produced anything.
//
// The pack is a conformant skill tree (one directory per skill, SKILL.md at its
// root) embedded with go:embed and read through the same loader that reads a pack
// from disk. Nothing about it is a special case: it is the format, from an fs.FS
// that happens to live in the executable.
//
// # Ownership
//
// The bundled scope belongs to the binary and to nothing else. Seed is the only
// writer, and it reconciles rather than merges: a slug the pack no longer ships is
// deleted, and a record whose content differs from the pack's is replaced. There is
// no surface for editing a bundled skill, and this is why. An edit would survive
// exactly until the next start, which is worse than not being offered. A user who
// wants a bundled skill changed copies it into a scope of their own, where the
// learning loop's guards already keep authored content safe.
//
// What is carried across an upgrade is the evidence: how often the skill was
// offered, how often a run read it, and how many of those runs won. That is what
// happened on this machine, not what the pack says, so replacing a skill's body
// keeps its record of how well it has performed rather than resetting its rank.
package bundled

import (
	"cmp"
	"context"
	"embed"
	"fmt"
	"io/fs"
	"slices"

	"github.com/ionalpha/flynn/skill"
	"github.com/ionalpha/flynn/skill/skillmd"
	"github.com/ionalpha/flynn/skill/skillrecall"
	"github.com/ionalpha/flynn/state"
)

// Root is the directory in the embedded filesystem that holds the pack. Each of its
// subdirectories is one skill, named for the skill it contains.
const Root = "skills"

//go:embed skills
var embedded embed.FS

// FS returns the embedded pack's filesystem. Resources a skill addresses (a script,
// a reference document) are read from here at execution time, which is why the
// filesystem is exported rather than being consumed entirely by Seed.
func FS() fs.FS { return embedded }

// Packs loads every skill directory in the pack. A pack that breaks the
// specification fails here rather than at seeding time, which is what the
// conformance test in this package exists to catch before a release does.
func Packs() ([]skillmd.Pack, error) { return skillmd.LoadAll(embedded, Root) }

// RetrievalPath is where a skill states the objectives it is reached for: a file in
// its own directory, so adding a skill to the pack stays an added directory and two
// people adding one at the same time do not have to merge each other's rows.
func RetrievalPath(slug string) string { return Root + "/" + slug + "/" + skillrecall.TableFile }

// RetrievalTable returns the objectives the pack claims each of its skills is
// reached for, and the objectives each must stay out of. The test in this package
// runs it against the real ranker, so a description that misses its own subject
// fails a build rather than going quiet in production.
func RetrievalTable() (skillrecall.Table, error) {
	t, err := skillrecall.LoadTable(embedded, Root)
	if err != nil {
		return skillrecall.Table{}, fmt.Errorf("bundled: %w", err)
	}
	return t, nil
}

// Skills returns the pack mapped into state.BundledScope, ordered by slug.
func Skills() ([]state.Skill, error) { return skillsFrom(embedded, Root) }

// skillsFrom is Skills over any skill tree. The filesystem is a parameter because
// the pack in the binary is one instance of a skill tree rather than a special kind
// of thing, and because a test needs a tree with skills in it before this one has
// any.
func skillsFrom(fsys fs.FS, root string) ([]state.Skill, error) {
	packs, err := skillmd.LoadAll(fsys, root)
	if err != nil {
		return nil, fmt.Errorf("bundled: load pack: %w", err)
	}
	out := make([]state.Skill, 0, len(packs))
	for _, p := range packs {
		sk, err := skill.FromPack(p, state.BundledScope)
		if err != nil {
			return nil, fmt.Errorf("bundled: %w", err)
		}
		out = append(out, sk)
	}
	slices.SortFunc(out, func(a, b state.Skill) int { return cmp.Compare(a.Slug, b.Slug) })
	return out, nil
}

// Result reports what a reconciliation did, by slug. It exists so a caller can say
// "3 new skills in this version" on an upgrade and stay silent on every start after
// it, rather than either announcing nothing or announcing the same set every time.
type Result struct {
	Added     []string
	Updated   []string
	Unchanged []string
	Removed   []string
}

// Changed reports whether the store differs from what it held before. An unchanged
// reconciliation is the normal case and the one worth staying quiet about.
func (r Result) Changed() bool { return len(r.Added)+len(r.Updated)+len(r.Removed) > 0 }

// Seed brings the bundled scope in store into line with the pack in this binary:
// skills it does not have are added, skills whose content has changed are replaced,
// and skills an older binary seeded that this one no longer ships are deleted.
//
// It runs on every start, not only the first. A binary is the authority on what it
// bundles, and an install that was upgraded in place has no other moment to notice.
// The steady state costs one list and no writes.
func Seed(ctx context.Context, store state.SkillStore) (Result, error) {
	want, err := Skills()
	if err != nil {
		return Result{}, err
	}
	return reconcile(ctx, store, want)
}

// Prune removes the whole bundled set, which is what running without it means. The
// set is derived from the binary, so deleting it loses nothing that cannot be seeded
// again by starting without the opt-out.
//
// Skipping the seed would not be the same thing: an install that has already run
// would keep the skills it was seeded earlier and go on recalling them, and a
// baseline measured against those is not a baseline.
func Prune(ctx context.Context, store state.SkillStore) (Result, error) {
	return reconcile(ctx, store, nil)
}

// reconcile makes the bundled scope hold exactly want.
func reconcile(ctx context.Context, store state.SkillStore, want []state.Skill) (Result, error) {
	var res Result
	have, err := store.List(ctx, state.BundledScope)
	if err != nil {
		return Result{}, fmt.Errorf("bundled: read seeded skills: %w", err)
	}
	stale := make(map[string]state.Skill, len(have))
	for _, sk := range have {
		stale[sk.Slug] = sk
	}

	for _, sk := range want {
		prev, seeded := stale[sk.Slug]
		delete(stale, sk.Slug)
		if seeded {
			sk.Offers, sk.Reads, sk.Wins = prev.Offers, prev.Reads, prev.Wins
			if sameContent(prev, sk) {
				res.Unchanged = append(res.Unchanged, sk.Slug)
				continue
			}
		}
		if _, err := store.Upsert(ctx, sk); err != nil {
			return res, fmt.Errorf("bundled: seed %s: %w", sk.Slug, err)
		}
		if seeded {
			res.Updated = append(res.Updated, sk.Slug)
		} else {
			res.Added = append(res.Added, sk.Slug)
		}
	}

	for _, sk := range sortedBySlug(stale) {
		// Deleted by id, never by slug: Delete resolves a slug across every scope and
		// would take the wrong record whenever a user's own skill shares the name.
		if err := store.Delete(ctx, sk.ID); err != nil {
			return res, fmt.Errorf("bundled: remove %s: %w", sk.Slug, err)
		}
		res.Removed = append(res.Removed, sk.Slug)
	}
	return res, nil
}

// sameContent reports whether a stored skill already says what the pack says.
// Everything the pack authors is compared; the fields it does not author (the
// identifiers, the timestamps, the outcome counters) are not.
func sameContent(stored, want state.Skill) bool {
	return stored.Name == want.Name &&
		stored.Description == want.Description &&
		stored.Body == want.Body &&
		stored.Check == want.Check &&
		slices.Equal(stored.Tags, want.Tags)
}

// sortedBySlug returns the map's skills in slug order, so a reconciliation reports
// the same thing twice in a row and a test can assert on it.
func sortedBySlug(m map[string]state.Skill) []state.Skill {
	out := make([]state.Skill, 0, len(m))
	for _, sk := range m {
		out = append(out, sk)
	}
	slices.SortFunc(out, func(a, b state.Skill) int { return cmp.Compare(a.Slug, b.Slug) })
	return out
}

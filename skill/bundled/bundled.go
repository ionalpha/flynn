// Package bundled embeds the authored skill pack in the binary and reconciles it into
// state.BundledScope, so a fresh install has skills before the learning loop runs.
// The pack is a conformant skill tree read by the same loader as a pack on disk.
//
// # Ownership
//
// [Seed] is the only writer of the pack's fields in the bundled scope. Each skill it
// writes carries [state.Skill.PackDigest], a fingerprint of what the pack wrote, so
// the next start can tell an untouched skill from one a host edited. An untouched
// skill takes the new pack content; an edited one keeps the edit, is marked
// [state.Skill.Diverged], and waits for a person to decide. A skill the pack dropped
// is deleted if untouched and released into the host's own scope if edited. See
// [Reconcile] for the rules and for installs seeded before the record existed.
// Offers, Reads and Wins are the host's and carry across an upgrade, so a new body
// keeps its rank.
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

// Root is the embedded directory holding one subdirectory per skill.
const Root = "skills"

//go:embed skills
var embedded embed.FS

// FS returns the embedded pack, from which a skill's scripts and references are read
// at execution time.
func FS() fs.FS { return embedded }

// Packs loads every skill directory. A pack that breaks the specification fails here,
// so this package's conformance test catches it before a release.
func Packs() ([]skillmd.Pack, error) { return skillmd.LoadAll(embedded, Root) }

// RetrievalPath is the file in a skill's own directory listing the objectives it is
// reached for, so two people adding skills never merge each other's rows.
func RetrievalPath(slug string) string { return Root + "/" + slug + "/" + skillrecall.TableFile }

// RetrievalTable assembles each skill's objectives to reach and to avoid. A test runs
// it against the real ranker, so a description that misses its subject fails the
// build. The error is unwrapped because it already names the file and line.
func RetrievalTable() (skillrecall.Table, error) { return skillrecall.LoadTable(embedded, Root) }

// Skills returns the pack mapped into state.BundledScope, ordered by slug.
func Skills() ([]state.Skill, error) { return skillsFrom(embedded, Root) }

// skillsFrom takes the filesystem so a test can supply its own tree.
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

// Result lists by slug what a reconciliation did, so a caller can announce an upgrade
// once and stay silent after.
type Result struct {
	// Added is skills the store did not have.
	Added []string
	// Updated is skills that took new pack content.
	Updated []string
	// Unchanged is skills already holding what the pack ships.
	Unchanged []string
	// Removed is skills the pack dropped that nobody had edited.
	Removed []string
	// Diverged is skills whose update was withheld for the first time this run.
	// A skill that stays diverged is not listed again; read [state.Skill.Diverged].
	Diverged []string
	// Released is dropped skills a host had edited, moved into the host's own scope.
	Released []string
	// Adopted is skills found with no record of what the pack wrote, which now have
	// one. A skill whose content differed from the pack also appears in Updated.
	Adopted []string
	// Cleared is skills whose divergence ended because the stored copy came to match
	// the pack.
	Cleared []string
}

// Changed reports whether this run changed the store in a way worth announcing.
// Adopted is bookkeeping and does not count.
func (r Result) Changed() bool {
	return len(r.Added)+len(r.Updated)+len(r.Removed)+len(r.Diverged)+len(r.Released)+len(r.Cleared) > 0
}

// Seed makes the bundled scope match this binary's pack through [Reconcile]: it adds
// skills, replaces the changed content of untouched ones, withholds the update from a
// skill a host edited, and removes what the pack no longer ships. It runs on every
// start because an in-place upgrade has no other moment to notice; the steady state
// is one list and no writes.
func Seed(ctx context.Context, store state.SkillStore) (Result, error) {
	want, err := Skills()
	if err != nil {
		return Result{}, err
	}
	return reconcile(ctx, store, want)
}

// Prune removes the whole bundled set; a later [Seed] restores it. Skipping Seed is
// insufficient because an install keeps and recalls what it was seeded earlier, which
// would spoil a baseline measured without bundled skills. A skill a host edited is
// released into the host's own scope instead of deleted, so it stays recallable: a
// baseline that must exclude it has to run on a store the host has not edited.
func Prune(ctx context.Context, store state.SkillStore) (Result, error) {
	return reconcile(ctx, store, nil)
}

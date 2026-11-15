// Package bundled embeds the authored skill pack in the binary and reconciles it into
// state.BundledScope, so a fresh install has skills before the learning loop runs.
// The pack is a conformant skill tree read by the same loader as a pack on disk.
//
// # Ownership
//
// [Seed] is the only writer of the bundled scope. It deletes slugs the pack dropped
// and replaces records that differ, so an edit to a bundled skill would last until
// the next start; there is no way to edit one. A user copies the skill into their own
// scope instead. Offers, Reads and Wins carry across an upgrade because they record
// this machine's runs, so a new body keeps its rank.
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
	Added     []string
	Updated   []string
	Unchanged []string
	Removed   []string
}

// Changed reports whether anything was added, updated or removed.
func (r Result) Changed() bool { return len(r.Added)+len(r.Updated)+len(r.Removed) > 0 }

// Seed makes the bundled scope match this binary's pack: it adds, replaces changed
// content, and deletes what the pack no longer ships. It runs on every start because
// an in-place upgrade has no other moment to notice; the steady state is one list
// and no writes.
func Seed(ctx context.Context, store state.SkillStore) (Result, error) {
	want, err := Skills()
	if err != nil {
		return Result{}, err
	}
	return reconcile(ctx, store, want)
}

// Prune removes the whole bundled set; a later [Seed] restores it. Skipping Seed is
// insufficient because an install keeps and recalls what it was seeded earlier, which
// would spoil a baseline measured without bundled skills.
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
		// By id, because Delete resolves a slug across scopes and could hit a user's
		// skill of the same name.
		if err := store.Delete(ctx, sk.ID); err != nil {
			return res, fmt.Errorf("bundled: remove %s: %w", sk.Slug, err)
		}
		res.Removed = append(res.Removed, sk.Slug)
	}
	return res, nil
}

// sameContent compares the fields the pack authors; ids, timestamps and counters are
// ignored.
func sameContent(stored, want state.Skill) bool {
	return stored.Name == want.Name &&
		stored.Description == want.Description &&
		stored.Body == want.Body &&
		stored.Check == want.Check &&
		slices.Equal(stored.Tags, want.Tags)
}

// sortedBySlug makes [Result.Removed] deterministic.
func sortedBySlug(m map[string]state.Skill) []state.Skill {
	out := make([]state.Skill, 0, len(m))
	for _, sk := range m {
		out = append(out, sk)
	}
	slices.SortFunc(out, func(a, b state.Skill) int { return cmp.Compare(a.Slug, b.Slug) })
	return out
}

package bundled

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"

	"github.com/ionalpha/flynn/state"
)

// Reconcile makes the bundled scope hold what want ships without overwriting a
// skill a host edited. [Seed] calls it with the embedded pack; a host that
// assembles its own pack calls it directly. Every skill in want is placed in
// [state.BundledScope].
//
// The pack owns name, description, body, tags and check. Everything else on a
// [state.Skill] is the host's and is never rewritten here: the Offers, Reads and Wins
// counters record this machine's runs, so a skill whose only change is a counter has
// not diverged from its pack. [state.Skill.PackDigest] holds a fingerprint of the
// pack-owned fields as the pack last wrote them, and a stored skill whose fields no
// longer produce it has been edited. For each skill in want:
//
//   - not stored: added, with the record.
//   - untouched and the pack changed: replaced, with the record moved to the new
//     content.
//   - edited and the pack changed: left as the host has it, and marked
//     [state.Skill.Diverged]. [Result.Diverged] names it the first time only; a person
//     decides, by editing the skill to match the pack or by deleting it so the next
//     run installs the pack's.
//   - edited and the stored copy now equals the pack: the mark is cleared and the
//     record moves to the pack's content.
//   - edited and the pack unchanged: nothing is withheld, so nothing is marked.
//
// A stored skill the pack no longer ships is deleted if untouched. If edited it is
// released: copied into the host's own scope with its counters and no record, then
// deleted from the bundled scope, so the host's work outlives the pack. If the host
// already has a skill of that slug the release is refused and the bundled copy is
// marked diverged, because overwriting either would lose work.
//
// An install seeded before records existed has skills with no digest, and nothing
// says whether they were edited. The first run adopts them: a skill that differs from
// the pack is replaced, as every earlier start did, and every one gets a record, so
// from the second run on an edit is protected. An edit made before that first run is
// not recoverable by any means this package has. [Result.Adopted] names these skills.
// A host that rewrites a seeded skill from scratch instead of reading it first drops
// the record and is adopted the same way.
//
// A write that loses a race with a host's own write ([state.ErrConflict]) is skipped
// and decided again on the next run, because the host's write may be an edit. Any
// other store error ends the run.
func Reconcile(ctx context.Context, store state.SkillStore, want []state.Skill) (Result, error) {
	return reconcile(ctx, store, want)
}

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

	want = slices.SortedFunc(slices.Values(want), func(a, b state.Skill) int { return cmp.Compare(a.Slug, b.Slug) })
	for _, sk := range want {
		sk.Scope = state.BundledScope
		prev, seeded := stale[sk.Slug]
		delete(stale, sk.Slug)
		if !seeded {
			if _, err := upsert(ctx, store, withRecord(sk, false), nil); err != nil {
				return res, fmt.Errorf("bundled: seed %s: %w", sk.Slug, err)
			}
			res.Added = append(res.Added, sk.Slug)
			continue
		}
		if err := place(ctx, store, &res, prev, sk); err != nil {
			return res, fmt.Errorf("bundled: seed %s: %w", sk.Slug, err)
		}
	}

	for _, sk := range sortedBySlug(stale) {
		if err := retire(ctx, store, &res, sk); err != nil {
			return res, fmt.Errorf("bundled: remove %s: %w", sk.Slug, err)
		}
	}
	return res, nil
}

// place decides one stored skill against the pack's version of it.
func place(ctx context.Context, store state.SkillStore, res *Result, prev, sk state.Skill) error {
	slug := sk.Slug
	newDigest := digest(sk)
	curDigest := digest(prev)
	adopt := withRecord(carry(sk, prev), false)

	// apply lists the slug once the write has gone through.
	apply := func(next state.Skill, list *[]string) error {
		raced, err := upsert(ctx, store, next, &prev)
		if err != nil || raced {
			return err
		}
		*list = append(*list, slug)
		return nil
	}

	switch {
	case prev.PackDigest == "":
		// Seeded before records existed; see Reconcile.
		if err := apply(adopt, &res.Adopted); err != nil {
			return err
		}
		if curDigest != newDigest && slices.Contains(res.Adopted, slug) {
			res.Updated = append(res.Updated, slug)
		}
	case curDigest == prev.PackDigest && prev.PackDigest != newDigest:
		// Untouched, and the pack moved.
		return apply(adopt, &res.Updated)
	case curDigest == prev.PackDigest:
		// Untouched, and the pack did not move.
		if prev.Diverged {
			return apply(adopt, &res.Cleared)
		}
		res.Unchanged = append(res.Unchanged, slug)
	case curDigest == newDigest:
		// Edited, and the edit is now exactly what the pack ships.
		if prev.Diverged {
			return apply(adopt, &res.Cleared)
		}
		return apply(adopt, &res.Unchanged)
	case prev.PackDigest == newDigest:
		// Edited, and the pack has nothing new to withhold.
		if prev.Diverged {
			return apply(withDiverged(prev, false), &res.Cleared)
		}
		res.Unchanged = append(res.Unchanged, slug)
	case !prev.Diverged:
		// Both sides moved. The record keeps the digest of what the pack last wrote, so
		// a host that reverts its edit is still recognised as untouched.
		return apply(withDiverged(prev, true), &res.Diverged)
	}
	return nil
}

// retire handles a stored skill the pack no longer ships.
func retire(ctx context.Context, store state.SkillStore, res *Result, sk state.Skill) error {
	if sk.PackDigest == "" || digest(sk) == sk.PackDigest {
		// By id, because Delete resolves a slug across scopes and could hit a user's
		// skill of the same name.
		if err := store.Delete(ctx, sk.ID); err != nil {
			return err
		}
		res.Removed = append(res.Removed, sk.Slug)
		return nil
	}

	own, err := store.List(ctx, state.Scope{})
	if err != nil {
		return err
	}
	for _, o := range own {
		if o.Slug != sk.Slug {
			continue
		}
		if digest(o) != digest(sk) {
			// Somebody else's skill holds the name. Both are kept.
			if sk.Diverged {
				return nil
			}
			if raced, err := upsert(ctx, store, withDiverged(sk, true), &sk); err != nil || raced {
				return err
			}
			res.Diverged = append(res.Diverged, sk.Slug)
			return nil
		}
		// An earlier run copied it and stopped before the delete.
		return releaseDone(ctx, store, res, sk)
	}

	released := state.Skill{
		Slug: sk.Slug, Name: sk.Name, Description: sk.Description, Body: sk.Body, Tags: sk.Tags, Check: sk.Check,
		Offers: sk.Offers, Reads: sk.Reads, Wins: sk.Wins,
	}
	if _, err := store.Upsert(ctx, released); err != nil {
		return err
	}
	return releaseDone(ctx, store, res, sk)
}

func releaseDone(ctx context.Context, store state.SkillStore, res *Result, sk state.Skill) error {
	if err := store.Delete(ctx, sk.ID); err != nil {
		return err
	}
	res.Released = append(res.Released, sk.Slug)
	return nil
}

// carry gives the pack's skill the stored one's host-owned fields.
func carry(sk, prev state.Skill) state.Skill {
	sk.Offers, sk.Reads, sk.Wins = prev.Offers, prev.Reads, prev.Wins
	return sk
}

// withRecord returns sk recording its own content as what the pack wrote.
func withRecord(sk state.Skill, diverged bool) state.Skill {
	sk.PackDigest = digest(sk)
	sk.Diverged = diverged
	return sk
}

// withDiverged flags or unflags a stored skill without touching its content or record.
func withDiverged(sk state.Skill, diverged bool) state.Skill {
	sk.Diverged = diverged
	return sk
}

// upsert stores sk. A non-nil prev makes the write conditional on prev still being
// current, and a lost race is reported as raced rather than as an error: the other
// writer may be an edit, so this run leaves the skill for the next one.
func upsert(ctx context.Context, store state.SkillStore, sk state.Skill, prev *state.Skill) (raced bool, err error) {
	if prev != nil {
		sk.ID, sk.SyncVersion = prev.ID, prev.SyncVersion
	}
	if _, err := store.Upsert(ctx, sk); err != nil {
		if errors.Is(err, state.ErrConflict) {
			return true, nil
		}
		return false, err
	}
	return false, nil
}

// digest fingerprints the pack-owned fields. Each is length-prefixed, so no two
// different field sets can produce the same bytes ("a"+"bc" against "ab"+"c"), and
// the tag count is written first so a tag cannot masquerade as a following field.
// Slug is included so a record cannot be carried to another skill. A nil and an empty
// tag list agree.
func digest(sk state.Skill) string {
	h := sha256.New()
	put := func(s string) {
		h.Write([]byte(strconv.Itoa(len(s))))
		h.Write([]byte{':'})
		h.Write([]byte(s))
	}
	put(sk.Slug)
	put(sk.Name)
	put(sk.Description)
	put(sk.Body)
	put(sk.Check)
	put(strconv.Itoa(len(sk.Tags)))
	for _, tag := range sk.Tags {
		put(tag)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// sortedBySlug makes the order of a run, and so [Result], deterministic.
func sortedBySlug(m map[string]state.Skill) []state.Skill {
	out := make([]state.Skill, 0, len(m))
	for _, sk := range m {
		out = append(out, sk)
	}
	slices.SortFunc(out, func(a, b state.Skill) int { return cmp.Compare(a.Slug, b.Slug) })
	return out
}

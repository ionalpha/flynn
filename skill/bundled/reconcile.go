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
// skill a host edited. [Seed] calls it with the embedded pack.
//
// The pack owns name, description, body, tags and check; the counters are the
// host's, so a counter change is not an edit. A skill is edited when its fields no
// longer produce [state.Skill.PackDigest]. An edited skill is withheld from an update
// and marked [state.Skill.Diverged] (named once in [Result.Diverged]) until the copies
// agree. A dropped skill is deleted if untouched and released into the host's own
// scope if edited, or marked diverged if that scope already has the slug.
//
// A skill with no digest predates records. earlier lists versions the pack shipped
// before ([Seed] reads them from pack_history.txt): a copy equal to one is replaced, any
// other is a host edit, kept and marked diverged. Either way it is recorded against
// want ([Result.Adopted]). A write that loses a race ([state.ErrConflict]) waits.
func Reconcile(ctx context.Context, store state.SkillStore, want, earlier []state.Skill) (Result, error) {
	shipped := make(digestSet, len(earlier))
	for _, sk := range earlier {
		sk.Scope = state.BundledScope
		shipped[digest(sk)] = struct{}{}
	}
	return reconcile(ctx, store, want, shipped)
}

// digestSet holds [digest] values of skills the pack has shipped.
type digestSet map[string]struct{}

func reconcile(ctx context.Context, store state.SkillStore, want []state.Skill, shipped digestSet) (Result, error) {
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
		if err := place(ctx, store, &res, prev, sk, shipped); err != nil {
			return res, fmt.Errorf("bundled: seed %s: %w", sk.Slug, err)
		}
	}

	for _, sk := range sortedBySlug(stale) {
		if err := retire(ctx, store, &res, sk, shipped); err != nil {
			return res, fmt.Errorf("bundled: remove %s: %w", sk.Slug, err)
		}
	}
	return res, nil
}

// place decides one stored skill against the pack's version of it. shipped is only
// read for a skill with no record.
func place(ctx context.Context, store state.SkillStore, res *Result, prev, sk state.Skill, shipped digestSet) error {
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
		if _, old := shipped[curDigest]; !old && curDigest != newDigest {
			// Recording the new digest, not the stored one, is what makes a later run
			// treat this copy as edited and keep withholding the pack's version.
			kept := withDiverged(prev, true)
			kept.PackDigest = newDigest
			if err := apply(kept, &res.Diverged); err != nil {
				return err
			}
			if slices.Contains(res.Diverged, slug) {
				res.Adopted = append(res.Adopted, slug)
			}
			return nil
		}
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

// retire handles a stored skill the pack no longer ships. A copy with no record is
// the pack's only when shipped holds it; any other is a host's edit and is released,
// the same verdict [place] reaches for one the pack still ships.
func retire(ctx context.Context, store state.SkillStore, res *Result, sk state.Skill, shipped digestSet) error {
	_, old := shipped[digest(sk)]
	if (sk.PackDigest == "" && old) || (sk.PackDigest != "" && digest(sk) == sk.PackDigest) {
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

package guard

import (
	"context"

	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/state"
)

// Eligibility is whether an item may be pushed unasked, in the wake digest every
// agent gets. A recalled answer is weighed by the reader who asked; a pushed line
// arrives on every wake with no question behind it. One poisoned pushed line is
// worth more to an attacker than any number of recallable ones, so the push set is a
// small subset of the recall set.
type Eligibility int

const (
	// PushOnPromotion is the zero value: it allows a push only after a trusted reviewer
	// promotes the item. It covers the agent's own untainted notes.
	PushOnPromotion Eligibility = iota
	// PushAllowed allows a push as is. It covers the operator's untainted
	// instructions.
	PushAllowed
	// PushDenied forbids a push whatever the promotion. The item stays recallable
	// with its provenance.
	PushDenied
)

func (e Eligibility) String() string {
	switch e {
	case PushAllowed:
		return "allowed"
	case PushDenied:
		return "denied"
	default:
		return "on-promotion"
	}
}

// PushEligibility classifies an item for the digest. It is pure, so the digest, a
// review queue and a backend pre-filter agree. In order:
//
//   - Tainted (state.MemoryItem.Tainted) is denied, because a reviewer of the final
//     sentence reads what the attacker wanted written.
//   - Untrusted provenance is denied.
//   - Operator-authored is allowed; anything else waits for promotion.
//
// Denied is terminal, because a model reviewer can itself be attacked.
func PushEligibility(it state.MemoryItem) Eligibility {
	if it.Tainted {
		return PushDenied
	}
	switch TrustOfAll(it.Sources) {
	case sandbox.TrustUntrusted:
		return PushDenied
	case sandbox.TrustTrusted:
		return PushAllowed
	default:
		return PushOnPromotion
	}
}

// PushEligible reports whether the item may be pushed given its promotion. Pass
// false when the promotion was not looked up.
func PushEligible(it state.MemoryItem, promoted bool) bool {
	switch PushEligibility(it) {
	case PushAllowed:
		return true
	case PushOnPromotion:
		return promoted
	default:
		return false
	}
}

// PromotionReader is the part of state.MemoryStore that [Pushable] needs, so a cache
// or test double can serve it.
type PromotionReader interface {
	Promotions(ctx context.Context, memoryIDs []string) ([]state.MemoryPromotion, error)
}

// Pushable returns the pushable items of in, reading promotions through store. The
// lookup is kept out of [PushEligibility] so that stays pure. Only items awaiting
// promotion are looked up, in at most one read.
func Pushable(ctx context.Context, store PromotionReader, in []state.MemoryItem) ([]state.MemoryItem, error) {
	var pending []string
	for _, it := range in {
		if PushEligibility(it) == PushOnPromotion {
			pending = append(pending, it.ID)
		}
	}
	promoted := map[string]bool{}
	if len(pending) > 0 {
		rows, err := store.Promotions(ctx, pending)
		if err != nil {
			return nil, err
		}
		promoted = state.PromotedSet(rows)
	}
	return FilterPushable(in, func(id string) bool { return promoted[id] }), nil
}

// FilterPushable returns the pushable items of in, in order. A nil promoted means
// nothing is promoted, which keeps only the operator's untainted items.
func FilterPushable(in []state.MemoryItem, promoted func(id string) bool) []state.MemoryItem {
	out := make([]state.MemoryItem, 0, len(in))
	for _, it := range in {
		ok := false
		if promoted != nil {
			ok = promoted(it.ID)
		}
		if PushEligible(it, ok) {
			out = append(out, it)
		}
	}
	return out
}

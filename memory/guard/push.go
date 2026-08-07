package guard

import (
	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/state"
)

// Eligibility is whether a memory item may be pushed at a reader unasked - put in
// the wake digest that every agent gets on every wake, rather than returned to a
// caller who searched for it.
//
// The distinction is the whole point of this file. A recall is a reader asking a
// question and getting an answer it can weigh; a push is content arriving in the
// prompt with no question behind it, on every wake, for as long as the item lives.
// One poisoned pushed line is therefore worth more to an attacker than any number
// of recallable ones, so the push set is a strict subset of the recall set and
// everything here is about keeping it small.
type Eligibility int

const (
	// PushOnPromotion means the item may be auto-pushed once a trusted reviewer has
	// promoted it, and not before. It is the zero value, and the answer for the
	// agent's own untainted notes: the bulk of what a curator writes, useful enough
	// to want in the digest and not vouched-for enough to put there unreviewed.
	PushOnPromotion Eligibility = iota
	// PushAllowed means the item may be auto-pushed as it stands. It is the operator's
	// own instruction, untainted: the principal's stated preference, which the agent
	// is meant to act on without being asked twice.
	PushAllowed
	// PushDenied means the item is never auto-pushed, and no promotion changes that.
	// It is recallable like anything else; a reader that goes looking for it gets it,
	// with its provenance, and decides what to do.
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

// PushEligibility classifies an item for the wake digest. It is a pure function of
// the stored record, so the digest builder, an operator's review queue, and a
// backend that wants to pre-filter its own rows cannot drift apart on what is
// pushable.
//
// The rules, in the order they apply:
//
//   - A tainted item is denied outright. Taint says untrusted input was in the
//     context that produced the fact (state.MemoryItem.Tainted), which is exactly the
//     laundering path a channel label cannot see, and no amount of review can
//     un-know it: a reviewer reading the finished sentence is reading what the
//     attacker wanted written.
//   - An untrusted-provenance item is denied. Content out of a tool, an inbound
//     message or a fetched page is attacker-influenceable by construction.
//   - An operator-authored item is allowed.
//   - Everything else - the agent's own notes, and anything whose provenance was
//     never recorded - waits for promotion.
//
// Denied is deliberately terminal rather than promotable. A promotion is one
// reviewer reading one item, and if that reviewer is an LLM the review is itself
// attack surface (the promotion prompt is a place the payload can aim at); letting
// it clear the two categories that are attacker-influenced by construction would
// put the whole gate inside the blast radius. Promotion moves the middle case only.
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

// PushEligible reports whether the item may be auto-pushed, given whether a trusted
// reviewer has promoted it. It is the predicate the digest builder filters on;
// promoted comes from the promotion record, and false is the safe answer for a
// caller that has not looked one up.
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

// FilterPushable returns the items of in that may be auto-pushed, in order.
// promoted answers whether an item id carries a promotion; a nil func is "nothing
// is promoted", which yields the operator's own untainted memories and nothing
// else - the smallest digest the policy allows and a usable default for a host with
// no promotion flow yet.
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

package state

import (
	"sort"
	"time"
)

// MemoryPromotion is a trusted reviewer's standing decision about one memory
// item: whether it may be pushed at a reader who did not ask for it. It is the
// gate in front of the wake digest, and the only way an item the agent wrote for
// itself gets in.
//
// It is a record beside the item, not a field on it: an item is append-only, so
// folding in a later, revisable decision would rewrite the fact each time a
// reviewer changed their mind. The row holds the current decision and the event
// stream holds every decision behind it (who decided, when, and what they said).
//
// Promotion is not trust. It cannot make a tainted or untrusted-origin item
// pushable (see the memory/guard package): a reviewer reads the finished
// sentence, and that is what an attacker writes. A promotion decides only the
// middle case, the agent's own untainted notes.
type MemoryPromotion struct {
	// MemoryID is the item this decision is about. It is a plain id, not a foreign
	// key: like usage, a promotion outlives its item's tombstone so a reviewer can
	// still see what was approved before it was retired.
	MemoryID string
	// Promoted is the current decision. False is a revocation when a row exists and
	// the default everywhere else: the digest treats a missing row and an explicit
	// "no" alike. They differ only in the audit trail, where one is silence and the
	// other a reviewer's answer.
	Promoted bool
	// By identifies the reviewer: the operator, or the curator acting under the
	// operator's policy. It is required, so every promotion traces to a decision.
	By string
	// Reason is the reviewer's note, optional, carried for the audit rather than
	// read by anything.
	Reason string
	// DecidedAt is when this decision was made. A revision moves it; the earlier
	// decisions keep their own timestamps on the event stream.
	DecidedAt time.Time
	Envelope
}

// PromotionDecision is one reviewer's call, the input to
// MemoryStore.Promote. It carries no timestamp or envelope: those are the store's
// to stamp, as with every other write here.
type PromotionDecision struct {
	// MemoryID is the item being decided about. It must name a live item.
	MemoryID string
	// Promoted is the decision: true admits the item to the push set, false keeps
	// it out or takes it back out.
	Promoted bool
	// By is the reviewer's identity. Required.
	By string
	// Reason is an optional note for the audit trail.
	Reason string
}

// Valid reports whether the decision names an item and a reviewer. Both are
// required and neither has a defensible default: a promotion with no item is
// meaningless, and one with no reviewer is the audit gap the record exists to
// close.
func (d PromotionDecision) Valid() bool { return d.MemoryID != "" && d.By != "" }

// PromotedSet reduces promotion rows to the ids that are currently promoted, the
// shape a digest's eligibility filter reads. A revoked row is absent from it, so a
// caller cannot accidentally treat "reviewed and refused" as "reviewed".
func PromotedSet(rows []MemoryPromotion) map[string]bool {
	out := make(map[string]bool, len(rows))
	for _, r := range rows {
		if r.Promoted {
			out[r.MemoryID] = true
		}
	}
	return out
}

// SortPromotions orders promotion rows by item id, so every backend returns one
// deterministic order.
func SortPromotions(rows []MemoryPromotion) {
	sort.Slice(rows, func(i, j int) bool { return rows[i].MemoryID < rows[j].MemoryID })
}

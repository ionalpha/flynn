package state

import (
	"sort"
	"time"
)

// MemoryPromotion is a reviewer's current decision on whether one memory item
// may be pushed at a reader who did not ask for it. It gates the wake digest,
// and is the only way an item the agent wrote for itself gets in.
//
// It is kept beside the item because items are append-only and a decision
// can be revised. The row holds the current decision; the event stream holds
// every earlier one, with who, when and why.
//
// A promotion cannot make a tainted or untrusted-origin item pushable (see the
// memory/guard package), because the sentence a reviewer reads is what an
// attacker writes. It decides only the agent's own untainted notes.
type MemoryPromotion struct {
	// MemoryID has no foreign key: a promotion outlives its item's tombstone so
	// a reviewer can still see what was approved.
	MemoryID string
	// Promoted is the current decision. The digest treats false and a missing
	// row alike; only the audit trail tells a revocation from silence.
	Promoted bool
	// By is the required reviewer: the operator, or the curator acting under the
	// operator's policy.
	By string
	// Reason is an optional note, kept for audit and read by nothing.
	Reason string
	// DecidedAt moves on revision; earlier decisions keep theirs on the stream.
	DecidedAt time.Time
	Envelope
}

// PromotionDecision is the input to MemoryStore.Promote. The store stamps the
// time and envelope.
type PromotionDecision struct {
	// MemoryID must name a live item.
	MemoryID string
	// Promoted true admits the item to the push set; false keeps or takes it out.
	Promoted bool
	// By is the reviewer, required.
	By     string
	Reason string
}

// Valid reports whether the decision names an item and a reviewer; neither
// has a default, since a decision with no reviewer leaves a gap in the audit.
func (d PromotionDecision) Valid() bool { return d.MemoryID != "" && d.By != "" }

// PromotedSet returns the ids currently promoted. Revoked rows are absent, so
// "reviewed and refused" cannot pass for "reviewed".
func PromotedSet(rows []MemoryPromotion) map[string]bool {
	out := make(map[string]bool, len(rows))
	for _, r := range rows {
		if r.Promoted {
			out[r.MemoryID] = true
		}
	}
	return out
}

// SortPromotions orders rows by item id, so every backend returns the same
// order.
func SortPromotions(rows []MemoryPromotion) {
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].MemoryID < rows[j].MemoryID })
}

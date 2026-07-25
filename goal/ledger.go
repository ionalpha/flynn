package goal

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// The ledger is a goal's expansion of its objective into the units of work that
// objective actually implies: what to do, and for each one the declared way to
// prove it was done. It is written by a planner phase before any building starts,
// it lives on the goal resource so it survives a crash and rehydrates on lease
// recovery, and it is the record completion is decided against.
//
// Completion being a state transition on this record, rather than a phrase matched
// in a transcript, is the point. Prose completion fails in both directions: a run
// declares victory on step one having written nothing, and a genuinely finished run
// goes undetected because its wording drifted. Neither is recoverable by parsing
// harder. A goal that cannot mark an item proven without saying which item and how
// has no way to express either failure.
//
// Two structural rules protect the record from the agent writing it:
//
// Append-and-mark-only. An item may be added, and an item's state may be marked,
// but no item is ever removed or rewritten. The failure this forecloses is the one
// where an agent under completion pressure edits the definition of done: dropping a
// test, narrowing an item until the thing it required is no longer required. A
// prompt asking the model not to do that is advisory and gets ignored under
// pressure; a ledger whose prefix cannot change is not.
//
// Content-addressed items. An item's ID is a hash of its text and its verify
// clause, so a rewrite is not a modified item, it is a different ID, and the
// extension check below sees it as the removal it is.

// ledgerIDLen is how many hex characters of an item's content hash form its ID.
// Sixteen (64 bits) is far past collision range for the handful of items one goal
// carries, and short enough to stay readable in a status dump.
const ledgerIDLen = 16

// Ledger errors. They are distinct because they mean different things about the
// writer: a regression is the definition of done being edited, an unknown item is a
// claim about work that was never planned, and an incomplete item is a plan entry
// with no way to check it.
var (
	// ErrLedgerRegressed reports that a proposed ledger is not an extension of the
	// one already recorded: an item was removed, reordered, or rewritten.
	ErrLedgerRegressed = errors.New("goal: ledger is not an append-only extension")
	// ErrLedgerUnknownItem reports a state change naming an item the ledger does not
	// carry.
	ErrLedgerUnknownItem = errors.New("goal: ledger item not found")
	// ErrLedgerIncomplete reports a ledger item missing its text or its verify clause.
	// An item with no declared way to check it cannot be proven, only asserted.
	ErrLedgerIncomplete = errors.New("goal: ledger item needs both an item and a verify")
	// ErrLedgerDuplicate reports the same item appended twice.
	ErrLedgerDuplicate = errors.New("goal: ledger item already present")
)

// LedgerItem is one planned unit of work. Verify is what makes the item checkable:
// the item's own declared way to prove it, recorded when the work is planned rather
// than argued for once it is time to claim the item is done.
type LedgerItem struct {
	// ID is the content address of Item and Verify (see ItemID). It is assigned on
	// append, not by the planner, so an item's identity is its content and a rewrite
	// cannot masquerade as an edit of the same item.
	ID string `json:"id"`
	// Item is what the work is, in the planner's words.
	Item string `json:"item"`
	// Verify is the declared way to prove the item: the command to run, the check to
	// make, the observation that would settle it.
	Verify string `json:"verify"`
}

// LedgerState is the observed state of one ledger item, keyed to it by ID. It
// starts unproven for every item, which is why the planner writes the whole ledger
// before step one: the run begins from a record that says nothing is done yet.
type LedgerState struct {
	ID     string `json:"id"`
	Proven bool   `json:"proven,omitempty"`
	// ProvenAt is when the item was marked, and Evidence is the reference to the
	// verification that proved it. Under the evidence gate (evidence.go) Evidence holds
	// the spine ref of the passing check the gate consumed to admit the proof, which is
	// what makes consumption durable: the set of spent refs is read back from the proven
	// items' Evidence, so a resumed run consumes exactly what the record already spent. A
	// proof recorded directly through MarkProven (a test, a replay) may carry a free-form
	// note here instead.
	ProvenAt *time.Time `json:"provenAt,omitempty"`
	Evidence string     `json:"evidence,omitempty"`
}

// ItemID returns the content address of an item: the first ledgerIDLen hex
// characters of a SHA-256 over its text and verify clause. The two are separated by
// a NUL so no pair of fields can be re-split into a different pair with the same
// concatenation.
func ItemID(item, verify string) string {
	sum := sha256.Sum256([]byte(item + "\x00" + verify))
	return hex.EncodeToString(sum[:])[:ledgerIDLen]
}

// AppendItems returns ledger with items appended, stamping each new item's ID from
// its content. It rejects an item missing its text or verify clause, and an item
// already on the ledger. The returned slice is new, so a caller that fails
// validation still holds an unmodified ledger.
func AppendItems(ledger []LedgerItem, items ...LedgerItem) ([]LedgerItem, error) {
	out := make([]LedgerItem, len(ledger), len(ledger)+len(items))
	copy(out, ledger)
	seen := make(map[string]struct{}, len(out)+len(items))
	for _, it := range out {
		seen[it.ID] = struct{}{}
	}
	for _, it := range items {
		it.Item = strings.TrimSpace(it.Item)
		it.Verify = strings.TrimSpace(it.Verify)
		if it.Item == "" || it.Verify == "" {
			return nil, fmt.Errorf("%w: %q", ErrLedgerIncomplete, it.Item)
		}
		it.ID = ItemID(it.Item, it.Verify)
		if _, dup := seen[it.ID]; dup {
			return nil, fmt.Errorf("%w: %q", ErrLedgerDuplicate, it.Item)
		}
		seen[it.ID] = struct{}{}
		out = append(out, it)
	}
	return out, nil
}

// ValidateExtension reports whether next is a legal successor to prev: prev must be
// a prefix of next, item for item and in order, and every item in next must carry
// the ID its own content addresses to. Anything else is a removal, a reordering, or
// a rewrite, and all three are the same failure wearing different clothes.
//
// This is the check that makes append-and-mark-only enforceable rather than
// requested. It runs on the write path, so an edited ledger is refused at the point
// of the write, and it runs again on reconcile, so a ledger edited around the write
// path settles the goal as stalled instead of being silently accepted.
func ValidateExtension(prev, next []LedgerItem) error {
	if len(next) < len(prev) {
		return fmt.Errorf("%w: %d items dropped", ErrLedgerRegressed, len(prev)-len(next))
	}
	seen := make(map[string]struct{}, len(next))
	for i, it := range next {
		if i < len(prev) {
			if was := prev[i]; it.ID != was.ID || it.Item != was.Item || it.Verify != was.Verify {
				return fmt.Errorf("%w: item %d (%s) was rewritten or reordered", ErrLedgerRegressed, i, was.ID)
			}
		}
		if it.Item == "" || it.Verify == "" {
			return fmt.Errorf("%w: item %d", ErrLedgerIncomplete, i)
		}
		if want := ItemID(it.Item, it.Verify); it.ID != want {
			return fmt.Errorf("%w: item %d carries id %q but addresses to %q", ErrLedgerRegressed, i, it.ID, want)
		}
		if _, dup := seen[it.ID]; dup {
			return fmt.Errorf("%w: item %d (%s)", ErrLedgerDuplicate, i, it.ID)
		}
		seen[it.ID] = struct{}{}
	}
	return nil
}

// ValidateLedger checks a goal's spec ledger against the state the status already
// records for it: the recorded entries must be a prefix of the ledger, in order and
// by id, and every ledger item must still address to its own id. It is the
// reconcile-time half of the extension rule. The write path refuses an edited
// ledger, and this refuses one that was edited around the write path, so a
// tampered ledger settles the goal as stalled rather than quietly becoming the new
// definition of done.
func (s Status) ValidateLedger(ledger []LedgerItem) error {
	if len(s.Ledger) > len(ledger) {
		return fmt.Errorf("%w: status records %d items, the ledger carries %d", ErrLedgerRegressed, len(s.Ledger), len(ledger))
	}
	for i, st := range s.Ledger {
		// s.Ledger is a prefix of the spec ledger, guaranteed by the length check above, so
		// this index is always in range; the explicit bound keeps that provable at the index
		// (matching ValidateExtension) rather than only across the early return.
		if i < len(ledger) && ledger[i].ID != st.ID {
			return fmt.Errorf("%w: item %d was %s, the ledger now carries %s", ErrLedgerRegressed, i, st.ID, ledger[i].ID)
		}
	}
	for i, it := range ledger {
		if it.Item == "" || it.Verify == "" {
			return fmt.Errorf("%w: item %d", ErrLedgerIncomplete, i)
		}
		if want := ItemID(it.Item, it.Verify); it.ID != want {
			return fmt.Errorf("%w: item %d carries id %q but addresses to %q", ErrLedgerRegressed, i, it.ID, want)
		}
	}
	return nil
}

// SyncLedger brings the status's per-item state into line with the spec's ledger:
// every planned item gains an unproven entry, in ledger order, and existing state
// is carried across untouched. It is called before the state is read or written, so
// a ledger that grew mid-run has state for its new items without any of them
// arriving pre-proven.
//
// State for an item no longer on the ledger is dropped rather than kept. That is not
// a way to lose a proof: the extension rule means an item cannot leave the ledger in
// the first place, so reaching this case at all means the ledger was already
// refused, and carrying orphan state forward would only let a proof survive the
// removal of the thing it proved.
func (s *Status) SyncLedger(ledger []LedgerItem) {
	if len(ledger) == 0 {
		s.Ledger = nil
		return
	}
	by := make(map[string]LedgerState, len(s.Ledger))
	for _, st := range s.Ledger {
		by[st.ID] = st
	}
	out := make([]LedgerState, 0, len(ledger))
	for _, it := range ledger {
		if st, ok := by[it.ID]; ok {
			out = append(out, st)
			continue
		}
		out = append(out, LedgerState{ID: it.ID})
	}
	s.Ledger = out
}

// MarkProven records an item as proven, with the evidence reference and the time it
// was marked. It reports ErrLedgerUnknownItem for an id the status carries no state
// for, so a claim about work that was never planned is a failure rather than a
// silently accepted new item. Marking an already-proven item again is a no-op that
// keeps the first proof: the record says when the item was first settled, not when
// it was last re-asserted.
func (s *Status) MarkProven(id, evidence string, now time.Time) error {
	for i := range s.Ledger {
		if s.Ledger[i].ID != id {
			continue
		}
		if s.Ledger[i].Proven {
			return nil
		}
		at := now
		s.Ledger[i].Proven = true
		s.Ledger[i].ProvenAt = &at
		s.Ledger[i].Evidence = evidence
		return nil
	}
	return fmt.Errorf("%w: %q", ErrLedgerUnknownItem, id)
}

// Unproven returns the ids of the items still unproven, in ledger order. An empty
// result on a non-empty ledger is what "every planned item is settled" looks like as
// a fact about a record rather than a sentence in a transcript.
func (s Status) Unproven() []string {
	var out []string
	for _, st := range s.Ledger {
		if !st.Proven {
			out = append(out, st.ID)
		}
	}
	return out
}

// LedgerSettled reports whether the ledger has items and all of them are proven. An
// empty ledger is not settled: a goal with nothing planned has not finished, it has
// not started.
func (s Status) LedgerSettled() bool {
	if len(s.Ledger) == 0 {
		return false
	}
	return len(s.Unproven()) == 0
}

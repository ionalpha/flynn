package goal

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ledgerIDLen is how many hex characters of an item's content hash form its ID: 64
// bits, past collision range for the items one goal carries.
const ledgerIDLen = 16

// Ledger errors are distinct because they say different things about the writer: a
// regression is the definition of done being edited, an unknown item is a claim about
// work never planned, and an incomplete item is a plan entry with no way to check it.
var (
	// ErrLedgerRegressed reports a proposed ledger that is not an extension of the
	// recorded one: an item was removed, reordered, or rewritten.
	ErrLedgerRegressed = errors.New("goal: ledger is not an append-only extension")
	// ErrLedgerUnknownItem reports a state change naming an item the ledger does not carry.
	ErrLedgerUnknownItem = errors.New("goal: ledger item not found")
	// ErrLedgerIncomplete reports an item missing its text or verify clause. An item with
	// no declared check can only be asserted, never proven.
	ErrLedgerIncomplete = errors.New("goal: ledger item needs both an item and a verify")
	// ErrLedgerDuplicate reports the same item appended twice.
	ErrLedgerDuplicate = errors.New("goal: ledger item already present")
)

// LedgerItem is one planned unit of work. The ledger expands a goal's objective into such
// units, written by a planner phase before any building starts and kept on the goal
// resource so it survives a crash. Completion is a state transition on this record and
// not a phrase matched in a transcript, which fails both ways: a run declares victory
// on step one having written nothing, or a finished run goes undetected because its
// wording drifted.
//
// Verify is the item's declared way to prove it, recorded when the work is planned and
// not argued for once it is time to claim the item is done.
type LedgerItem struct {
	// ID is the content address of Item and Verify (see [ItemID]). It is assigned on
	// append, not by the planner, so a rewrite cannot masquerade as an edit of the same
	// item.
	ID string `json:"id"`
	// Item is what the work is, in the planner's words.
	Item string `json:"item"`
	// Verify is the declared way to prove the item: the command to run, the observation
	// that would settle it.
	Verify string `json:"verify"`
}

// LedgerState is the observed state of one ledger item, keyed to it by ID. Every item
// starts unproven, so the run begins from a record saying nothing is done yet.
type LedgerState struct {
	ID     string `json:"id"`
	Proven bool   `json:"proven,omitempty"`
	// ProvenAt is when the item was marked, and Evidence references the verification that
	// proved it. Under the evidence gate (evidence.go) Evidence holds the spine ref of the
	// passing check the gate consumed, and the spent refs are read back from the proven
	// items' Evidence, so a resumed run consumes exactly what the record already spent. A
	// proof recorded through [Status.MarkProven] (a test, a replay) may carry a free-form
	// note instead.
	ProvenAt *time.Time `json:"provenAt,omitempty"`
	Evidence string     `json:"evidence,omitempty"`
}

// ItemID returns the content address of an item: the first ledgerIDLen hex characters of
// a SHA-256 over its text and verify clause, NUL-separated so no pair of fields re-splits
// into a different pair with the same concatenation. A rewrite gets a different ID, so
// [ValidateExtension] sees the removal it is.
func ItemID(item, verify string) string {
	sum := sha256.Sum256([]byte(item + "\x00" + verify))
	return hex.EncodeToString(sum[:])[:ledgerIDLen]
}

// AppendItems returns ledger with items appended, stamping each new item's ID from its
// content. It rejects an item missing its text or verify clause, and one already on the
// ledger. The returned slice is new, so a caller whose validation fails still holds an
// unmodified ledger.
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

// PlanExtension returns ledger with items appended, first dropping any incoming item
// whose content already addresses to an id the ledger carries. A planning step that
// crashed after writing the ledger but before completing its job re-runs and re-proposes
// the items it recorded; dropping those exact re-statements makes the re-run a no-op and
// not an [ErrLedgerDuplicate] stalling a goal whose ledger is already correct.
//
// Only an exact re-statement is dropped. An item reworded to a new id is a new item and
// is appended; this cannot detect that. New items still pass through [AppendItems], so
// the same new item twice in one batch is still refused.
func PlanExtension(ledger []LedgerItem, items ...LedgerItem) ([]LedgerItem, error) {
	have := make(map[string]struct{}, len(ledger))
	for _, it := range ledger {
		have[it.ID] = struct{}{}
	}
	fresh := make([]LedgerItem, 0, len(items))
	for _, it := range items {
		if _, ok := have[ItemID(strings.TrimSpace(it.Item), strings.TrimSpace(it.Verify))]; ok {
			continue
		}
		fresh = append(fresh, it)
	}
	return AppendItems(ledger, fresh...)
}

// ValidateExtension reports whether next is a legal successor to prev: prev must be a
// prefix of next, item for item and in order, and every item in next must carry the ID
// its own content addresses to. Anything else is a removal, a reordering or a rewrite.
//
// This makes the ledger append-and-mark-only by enforcement and not request. An agent
// under completion pressure may edit the definition of done (dropping a test, narrowing
// an item), and a prompt asking it not to is advisory. The check runs on the write path,
// and again on reconcile, so a ledger edited around the write path settles the goal as
// stalled.
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

// ValidateLedger checks a goal's spec ledger against the state the status records for
// it: the recorded entries must be a prefix of the ledger, in order and by id, and every
// ledger item must still address to its own id. It is the reconcile-time half of the
// extension rule, refusing a ledger edited around the write path.
func (s Status) ValidateLedger(ledger []LedgerItem) error {
	if len(s.Ledger) > len(ledger) {
		return fmt.Errorf("%w: status records %d items, the ledger carries %d", ErrLedgerRegressed, len(s.Ledger), len(ledger))
	}
	for i, st := range s.Ledger {
		// The length check above makes i in range; the explicit bound keeps that provable
		// here, as in ValidateExtension.
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

// SyncLedger brings the status's per-item state into line with the spec's ledger: every
// planned item gains an unproven entry, in ledger order, and existing state is carried
// across untouched. A ledger that grew mid-run thus has state for its new items, none
// arriving pre-proven.
//
// State for an item no longer on the ledger is dropped. The extension rule means an item
// cannot leave the ledger, so reaching this case means the ledger was already refused,
// and carrying orphan state would let a proof outlive the thing it proved.
func (s *Status) SyncLedger(ledger []LedgerItem) {
	s.Ledger = syncStateByID(s.Ledger, ledger,
		func(st LedgerState) string { return st.ID },
		func(it LedgerItem) string { return it.ID },
		func(id string) LedgerState { return LedgerState{ID: id} })
}

// MarkProven records an item as proven, with the evidence reference and the time it was
// marked. It reports [ErrLedgerUnknownItem] for an id the status has no state for, so a
// claim about work never planned fails and is not accepted as a new item. Marking a
// proven item again is a no-op that keeps the first proof: the record says when the item
// was first settled, not when it was last re-asserted.
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

// Unproven returns the ids of the items still unproven, in ledger order.
func (s Status) Unproven() []string {
	var out []string
	for _, st := range s.Ledger {
		if !st.Proven {
			out = append(out, st.ID)
		}
	}
	return out
}

// CurrentItem returns the item the run is working: the first ledger item still unproven.
// It reports false when the ledger is empty or every item is proven.
//
// The selection is derived and not stored on the status, so there is no second
// representation of "where the run is" to drift from the record. The reconciler, the
// executor and the producer all compute the same answer from the same two facts.
func (s Status) CurrentItem(ledger []LedgerItem) (LedgerItem, bool) {
	proven := make(map[string]bool, len(s.Ledger))
	for _, st := range s.Ledger {
		proven[st.ID] = st.Proven
	}
	for _, it := range ledger {
		if !proven[it.ID] {
			return it, true
		}
	}
	return LedgerItem{}, false
}

// LedgerSettled reports whether the ledger has items and all of them are proven. An empty
// ledger is not settled: a goal with nothing planned has not started.
func (s Status) LedgerSettled() bool {
	if len(s.Ledger) == 0 {
		return false
	}
	return len(s.Unproven()) == 0
}

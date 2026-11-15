package goal

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ledgerIDLen is 64 bits, past collision range for one goal's items.
const ledgerIDLen = 16

var (
	// ErrLedgerRegressed reports an item removed, reordered or rewritten.
	ErrLedgerRegressed = errors.New("goal: ledger is not an append-only extension")
	// ErrLedgerUnknownItem reports a state change naming an item the ledger does not carry.
	ErrLedgerUnknownItem = errors.New("goal: ledger item not found")
	// ErrLedgerIncomplete reports an item with no verify clause, which can never be proven.
	ErrLedgerIncomplete = errors.New("goal: ledger item needs both an item and a verify")
	// ErrLedgerDuplicate reports the same item appended twice.
	ErrLedgerDuplicate = errors.New("goal: ledger item already present")
)

// LedgerItem is one planned unit of work, written before building and kept on the goal so
// it survives a crash. Completion is a state change on this record, because transcript
// phrases both claim early and miss a finish. Verify is fixed at planning, so it is not
// argued for at claim time.
type LedgerItem struct {
	// ID is [ItemID], assigned on append, so a rewrite cannot pass as an edit.
	ID   string `json:"id"`
	Item string `json:"item"`
	// Verify is the command or observation that proves the item.
	Verify string `json:"verify"`
}

// LedgerState starts unproven, so a run begins from a record saying nothing is done.
type LedgerState struct {
	ID     string `json:"id"`
	Proven bool   `json:"proven,omitempty"`
	// Evidence is the spine ref the evidence gate consumed, and spent refs are read back
	// from it, so a resumed run spends nothing twice. [Status.MarkProven] may store a note.
	ProvenAt *time.Time `json:"provenAt,omitempty"`
	Evidence string     `json:"evidence,omitempty"`
	// Abandoned is set only when proof is advisory ([WithLedgerConvergence]), because the
	// model's own pre-work check may never pass. The item stays unproven, so the record
	// still shows the plan unproven; the mark only moves the run on.
	Abandoned       bool   `json:"abandoned,omitempty"`
	AbandonedReason string `json:"abandonedReason,omitempty"`
}

// ItemID hashes text and verify NUL-separated, so no two pairs share bytes. A rewrite
// gets a new ID, so [ValidateExtension] sees it as a removal.
func ItemID(item, verify string) string {
	sum := sha256.Sum256([]byte(item + "\x00" + verify))
	return hex.EncodeToString(sum[:])[:ledgerIDLen]
}

// AppendItems returns a new slice, so a failed append leaves the caller's ledger intact.
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

// PlanExtension drops items already on the ledger, so a planning step re-run after a
// crash is a no-op instead of [ErrLedgerDuplicate]. A reworded item is appended as new.
// The same new item twice in one batch is still refused.
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

// ValidateExtension requires prev as a prefix of next and every ID to match its content,
// because a run under completion pressure may narrow the definition of done. It runs on
// write and again on reconcile, so an edit around the write path stalls the goal.
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

// ValidateLedger is the reconcile-time half of [ValidateExtension].
func (s Status) ValidateLedger(ledger []LedgerItem) error {
	if len(s.Ledger) > len(ledger) {
		return fmt.Errorf("%w: status records %d items, the ledger carries %d", ErrLedgerRegressed, len(s.Ledger), len(ledger))
	}
	for i, st := range s.Ledger {
		// The bound is redundant with the length check and keeps it provable here.
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

// SyncLedger gives new items unproven entries. State for an item off the ledger is
// dropped, so a proof cannot outlive what it proved.
func (s *Status) SyncLedger(ledger []LedgerItem) {
	s.Ledger = syncStateByID(s.Ledger, ledger,
		func(st LedgerState) string { return st.ID },
		func(it LedgerItem) string { return it.ID },
		func(id string) LedgerState { return LedgerState{ID: id} })
}

// MarkProven refuses an unplanned id with [ErrLedgerUnknownItem]. Marking again keeps the
// first proof, so the record shows when the item first settled.
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

// CurrentItem is the first item neither proven nor abandoned. It is derived and never
// stored, so no second copy of the run's position can drift from the record.
func (s Status) CurrentItem(ledger []LedgerItem) (LedgerItem, bool) {
	done := make(map[string]bool, len(s.Ledger))
	for _, st := range s.Ledger {
		done[st.ID] = st.Proven || st.Abandoned
	}
	for _, it := range ledger {
		if !done[it.ID] {
			return it, true
		}
	}
	return LedgerItem{}, false
}

// Abandoned returns the items the run moved past without proving, in ledger order.
func (s Status) Abandoned() []LedgerState {
	var out []LedgerState
	for _, st := range s.Ledger {
		if st.Abandoned && !st.Proven {
			out = append(out, st)
		}
	}
	return out
}

// abandonCurrent clears the check counts, because the next item's first failure would
// otherwise read as the abandoned item's third and stop the run.
func (s *Status) abandonCurrent(ledger []LedgerItem, reason string) {
	item, ok := s.CurrentItem(ledger)
	if !ok {
		return
	}
	for i := range s.Ledger {
		if s.Ledger[i].ID == item.ID {
			s.Ledger[i].Abandoned = true
			s.Ledger[i].AbandonedReason = reason
		}
	}
	s.ItemFeedback = ""
	s.MissingCommand, s.MissingRepeat = "", 0
	s.LastVerdict, s.VerdictMark, s.VerdictRepeat = "", "", 0
}

// LedgerSettled is false for an empty ledger, because nothing planned means not started.
func (s Status) LedgerSettled() bool {
	if len(s.Ledger) == 0 {
		return false
	}
	return len(s.Unproven()) == 0
}

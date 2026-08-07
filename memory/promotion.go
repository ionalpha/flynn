package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ionalpha/flynn/envelope"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/state"
)

// promotionSpecSchema is the JSON Schema a promotion record's Spec must satisfy.
// The reviewer is required for the same reason the contract requires it: a
// decision nobody is named on cannot be audited back to anyone.
var promotionSpecSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "memory_id": {"type": "string", "minLength": 1},
    "promoted": {"type": "boolean"},
    "decided_by": {"type": "string", "minLength": 1},
    "reason": {"type": "string"},
    "decided_at": {"type": "string"}
  },
  "required": ["memory_id", "decided_by"],
  "additionalProperties": false
}`)

// promotionSpec is the stored shape of a state.MemoryPromotion. DecidedAt is a
// pointer for consistency with the other optional times on this facade, though a
// stamped decision always carries one.
type promotionSpec struct {
	MemoryID  string     `json:"memory_id"`
	Promoted  bool       `json:"promoted,omitempty"`
	By        string     `json:"decided_by"`
	Reason    string     `json:"reason,omitempty"`
	DecidedAt *time.Time `json:"decided_at,omitempty"`
}

// promotionName is the natural name of the decision record for one item. Unlike
// usage there is no instance in the key: a promotion is a decision about the item,
// not an observation by an instance, and one item has one answer in force.
func promotionName(memoryID string) string { return promotionNamePrefix + memoryID }

// Promote records a reviewer's decision about an item's push eligibility, creating
// the record or replacing the decision in force. The item is resolved first, so a
// decision about an unknown or tombstoned id is refused rather than filed against
// nothing.
func (s *Store) Promote(ctx context.Context, d state.PromotionDecision) (state.MemoryPromotion, error) {
	if !d.Valid() {
		return state.MemoryPromotion{}, state.ErrInvalid
	}
	item, err := s.rs.GetByID(ctx, d.MemoryID)
	if err != nil {
		return state.MemoryPromotion{}, translateErr(err)
	}
	now := s.clk.Now()
	sp := promotionSpec{MemoryID: d.MemoryID, Promoted: d.Promoted, By: d.By, Reason: d.Reason, DecidedAt: &now}
	body, err := json.Marshal(sp)
	if err != nil {
		return state.MemoryPromotion{}, fmt.Errorf("memory: encode promotion spec: %w", err)
	}
	// The stored version carries into the write, so a decision made against a row
	// somebody else has since revised loses the write rather than silently
	// overwriting a newer reviewer's answer.
	var syncVersion int64
	name := promotionName(d.MemoryID)
	switch cur, err := s.rs.Get(ctx, PromotionKind, item.Scope, name); {
	case err == nil:
		syncVersion = cur.SyncVersion
	case errors.Is(err, resource.ErrNotFound):
	default:
		return state.MemoryPromotion{}, translateErr(err)
	}
	out, err := s.rs.Put(ctx, resource.Resource{
		APIVersion: GroupVersion,
		Kind:       PromotionKind,
		Name:       name,
		Scope:      item.Scope,
		Spec:       body,
		Envelope: resource.Envelope{
			Envelope: envelope.Envelope{SyncVersion: syncVersion},
		},
	})
	if err != nil {
		return state.MemoryPromotion{}, translateErr(err)
	}
	return toPromotion(out)
}

// Promotions returns the decision records for these items, or every record the
// store holds when no ids are given. Like Usage it reads across scopes and filters
// here, because the caller asks about a whole digest at once.
func (s *Store) Promotions(ctx context.Context, memoryIDs []string) ([]state.MemoryPromotion, error) {
	rs, err := s.rs.ListAll(ctx, PromotionKind, nil)
	if err != nil {
		return nil, translateErr(err)
	}
	var want map[string]bool
	if len(memoryIDs) > 0 {
		want = make(map[string]bool, len(memoryIDs))
		for _, id := range memoryIDs {
			want[id] = true
		}
	}
	out := make([]state.MemoryPromotion, 0, len(rs))
	for _, r := range rs {
		p, err := toPromotion(r)
		if err != nil {
			return nil, err
		}
		if want != nil && !want[p.MemoryID] {
			continue
		}
		out = append(out, p)
	}
	state.SortPromotions(out)
	return out, nil
}

// toPromotion maps a promotion resource back to the typed record.
func toPromotion(r resource.Resource) (state.MemoryPromotion, error) {
	sp, err := resource.DecodeSpec[promotionSpec](r)
	if err != nil {
		return state.MemoryPromotion{}, fmt.Errorf("memory: decode promotion spec: %w", err)
	}
	p := state.MemoryPromotion{
		MemoryID: sp.MemoryID,
		Promoted: sp.Promoted,
		By:       sp.By,
		Reason:   sp.Reason,
		Envelope: state.Envelope{
			SyncVersion:      r.SyncVersion,
			OriginInstanceID: r.OriginInstanceID,
			UpdatedHLC:       r.UpdatedHLC,
			LastWriterID:     r.LastWriterID,
			Deleted:          r.Deleted,
		},
	}
	if sp.DecidedAt != nil {
		p.DecidedAt = *sp.DecidedAt
	}
	return p, nil
}

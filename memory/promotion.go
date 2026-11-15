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

// promotionSpecSchema requires the reviewer, so every decision can be audited.
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

// promotionSpec stores a state.MemoryPromotion.
type promotionSpec struct {
	MemoryID  string     `json:"memory_id"`
	Promoted  bool       `json:"promoted,omitempty"`
	By        string     `json:"decided_by"`
	Reason    string     `json:"reason,omitempty"`
	DecidedAt *time.Time `json:"decided_at,omitempty"`
}

func promotionName(memoryID string) string { return promotionNamePrefix + memoryID }

// Promote records or replaces a reviewer's push-eligibility decision. An unknown or
// tombstoned item is refused.
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
	// The stored version goes into the write, so a stale decision conflicts and
	// cannot overwrite a newer one.
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

// Promotions returns the decisions for these items, or all of them for no ids.
func (s *Store) Promotions(ctx context.Context, memoryIDs []string) ([]state.MemoryPromotion, error) {
	out, err := listByMemoryID(ctx, s, PromotionKind, memoryIDs, toPromotion, func(p state.MemoryPromotion) string { return p.MemoryID })
	if err != nil {
		return nil, err
	}
	state.SortPromotions(out)
	return out, nil
}

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

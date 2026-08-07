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

// defaultInstanceID is the instance usage is attributed to when the caller sets
// none. It matches the in-memory state provider's default, so the two backends
// attribute the same reads to the same name in a test that runs both.
const defaultInstanceID = "local"

// usageWriteAttempts bounds the read-modify-write retry on a usage record. Only
// this instance writes its own record, so a conflict here means two of its own
// goroutines recorded at once; one retry apiece settles that, and a store that
// keeps conflicting is reporting a real problem rather than contention to spin on.
const usageWriteAttempts = 3

// usageSpecSchema is the JSON Schema a usage record's Spec must satisfy. The
// counters are unbounded but never negative: a usage record only ever counts up.
var usageSpecSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "memory_id": {"type": "string", "minLength": 1},
    "instance_id": {"type": "string", "minLength": 1},
    "push_count": {"type": "integer", "minimum": 0},
    "last_pushed_at": {"type": "string"},
    "organic_uses": {"type": "integer", "minimum": 0},
    "primed_uses": {"type": "integer", "minimum": 0},
    "last_used_at": {"type": "string"}
  },
  "required": ["memory_id", "instance_id"],
  "additionalProperties": false
}`)

// usageSpec is the stored shape of a state.MemoryUsage. The timestamps are
// pointers so a record that has only ever been pushed omits the used-at key
// entirely, rather than encoding the zero time and hashing as though something
// had happened at the start of the epoch.
type usageSpec struct {
	MemoryID     string     `json:"memory_id"`
	InstanceID   string     `json:"instance_id"`
	PushCount    int64      `json:"push_count,omitempty"`
	LastPushedAt *time.Time `json:"last_pushed_at,omitempty"`
	OrganicUses  int64      `json:"organic_uses,omitempty"`
	PrimedUses   int64      `json:"primed_uses,omitempty"`
	LastUsedAt   *time.Time `json:"last_used_at,omitempty"`
}

// usageName is the natural name of the usage record for one item on one instance.
func usageName(memoryID, instanceID string) string {
	return usageNamePrefix + memoryID + "-" + instanceID
}

// RecordPush counts one push of each item against this instance. Every id is
// resolved first, so a set carrying an unknown or tombstoned id records nothing.
//
// The records are then written one at a time, because a resource store commits one
// resource at a time. A store that fails midway leaves the earlier items counted,
// which is the honest failure for a counter: the push it recorded did happen.
func (s *Store) RecordPush(ctx context.Context, memoryIDs []string) error {
	if len(memoryIDs) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(memoryIDs))
	items := make([]resource.Resource, 0, len(memoryIDs))
	for _, id := range memoryIDs {
		// A repeated id counts once: one digest cannot push the same item twice, so
		// counting it twice would record a caller's slip as an observation.
		if seen[id] {
			continue
		}
		seen[id] = true
		r, err := s.rs.GetByID(ctx, id)
		if err != nil {
			return translateErr(err)
		}
		items = append(items, r)
	}
	now := s.clk.Now()
	for _, r := range items {
		err := s.mutateUsage(ctx, r.ID, r.Scope, func(u *usageSpec) {
			u.PushCount++
			u.LastPushedAt = &now
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// RecordUse counts one use of an item at the given origin against this instance.
func (s *Store) RecordUse(ctx context.Context, memoryID string, origin state.UsageOrigin) error {
	if !origin.Valid() {
		return state.ErrInvalid
	}
	r, err := s.rs.GetByID(ctx, memoryID)
	if err != nil {
		return translateErr(err)
	}
	now := s.clk.Now()
	return s.mutateUsage(ctx, r.ID, r.Scope, func(u *usageSpec) {
		if origin == state.UsagePrimed {
			u.PrimedUses++
		} else {
			u.OrganicUses++
		}
		u.LastUsedAt = &now
	})
}

// Usage returns the per-instance usage records for these items, or every record
// the store holds when no ids are given.
//
// It reads across scopes and filters here rather than resolving each item's scope
// first: a usage record lives in its item's scope, so a per-item read would be one
// lookup plus one scoped list per item to answer a question the caller asks about
// a whole digest at once.
func (s *Store) Usage(ctx context.Context, memoryIDs []string) ([]state.MemoryUsage, error) {
	rs, err := s.rs.ListAll(ctx, UsageKind, nil)
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
	out := make([]state.MemoryUsage, 0, len(rs))
	for _, r := range rs {
		u, err := toUsage(r)
		if err != nil {
			return nil, err
		}
		if want != nil && !want[u.MemoryID] {
			continue
		}
		out = append(out, u)
	}
	state.SortUsage(out)
	return out, nil
}

// mutateUsage applies mutate to this instance's usage record for an item and
// stores it, creating the record if this instance has not touched the item before.
// The read carries its sync version into the write, so a concurrent recorder loses
// the write rather than the increment, and the loop then re-reads and reapplies.
func (s *Store) mutateUsage(ctx context.Context, memoryID string, scope resource.Scope, mutate func(*usageSpec)) error {
	name := usageName(memoryID, s.instanceID)
	var lastErr error
	for range usageWriteAttempts {
		sp := usageSpec{MemoryID: memoryID, InstanceID: s.instanceID}
		var syncVersion int64
		cur, err := s.rs.Get(ctx, UsageKind, scope, name)
		switch {
		case err == nil:
			if sp, err = resource.DecodeSpec[usageSpec](cur); err != nil {
				return fmt.Errorf("memory: decode usage spec: %w", err)
			}
			syncVersion = cur.SyncVersion
		case errors.Is(err, resource.ErrNotFound):
		default:
			return translateErr(err)
		}
		mutate(&sp)
		body, err := json.Marshal(sp)
		if err != nil {
			return fmt.Errorf("memory: encode usage spec: %w", err)
		}
		out, err := s.rs.Put(ctx, resource.Resource{
			APIVersion: GroupVersion,
			Kind:       UsageKind,
			Name:       name,
			Scope:      scope,
			Spec:       body,
			Envelope: resource.Envelope{
				Envelope: envelope.Envelope{SyncVersion: syncVersion},
			},
		})
		if errors.Is(err, resource.ErrConflict) {
			lastErr = state.ErrConflict
			continue
		}
		if err != nil {
			return translateErr(err)
		}
		// The record is keyed by the instance it belongs to, so a store that
		// attributes the write to a different instance would file this instance's
		// reads under a name nothing else uses. Fail here, where it is one
		// misconfigured option, rather than in a metric that quietly halves.
		if out.OriginInstanceID != "" && out.OriginInstanceID != s.instanceID {
			return fmt.Errorf("memory: usage attributed to instance %q but this store records as %q: %w",
				out.OriginInstanceID, s.instanceID, state.ErrInvalid)
		}
		return nil
	}
	return lastErr
}

// toUsage maps a usage resource back to the typed record, carrying the shared
// envelope across as every other read on this facade does.
func toUsage(r resource.Resource) (state.MemoryUsage, error) {
	sp, err := resource.DecodeSpec[usageSpec](r)
	if err != nil {
		return state.MemoryUsage{}, fmt.Errorf("memory: decode usage spec: %w", err)
	}
	u := state.MemoryUsage{
		MemoryID:    sp.MemoryID,
		InstanceID:  sp.InstanceID,
		PushCount:   sp.PushCount,
		OrganicUses: sp.OrganicUses,
		PrimedUses:  sp.PrimedUses,
		Envelope: state.Envelope{
			SyncVersion:      r.SyncVersion,
			OriginInstanceID: r.OriginInstanceID,
			UpdatedHLC:       r.UpdatedHLC,
			LastWriterID:     r.LastWriterID,
			Deleted:          r.Deleted,
		},
	}
	if sp.LastPushedAt != nil {
		u.LastPushedAt = *sp.LastPushedAt
	}
	if sp.LastUsedAt != nil {
		u.LastUsedAt = *sp.LastUsedAt
	}
	return u, nil
}

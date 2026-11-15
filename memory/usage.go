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

// defaultInstanceID matches the in-memory state provider's default, so a test
// running both backends sees one name.
const defaultInstanceID = "local"

// usageWriteAttempts bounds the retry on a usage record. Only this instance writes
// its record, so a conflict is two of its goroutines, and repeated conflicts are a
// fault to report.
const usageWriteAttempts = 3

// usageSpecSchema keeps counters non-negative; usage only counts up.
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

// usageSpec stores a state.MemoryUsage. The times are pointers so an unset one is
// omitted instead of encoded as the zero time.
type usageSpec struct {
	MemoryID     string     `json:"memory_id"`
	InstanceID   string     `json:"instance_id"`
	PushCount    int64      `json:"push_count,omitempty"`
	LastPushedAt *time.Time `json:"last_pushed_at,omitempty"`
	OrganicUses  int64      `json:"organic_uses,omitempty"`
	PrimedUses   int64      `json:"primed_uses,omitempty"`
	LastUsedAt   *time.Time `json:"last_used_at,omitempty"`
}

func usageName(memoryID, instanceID string) string {
	return usageNamePrefix + memoryID + "-" + instanceID
}

// RecordPush counts one push of each item against this instance. Every id resolves
// first, so an unknown or tombstoned id records nothing. Records are written one at
// a time, so a failure midway leaves earlier pushes counted, which did happen.
func (s *Store) RecordPush(ctx context.Context, memoryIDs []string) error {
	if len(memoryIDs) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(memoryIDs))
	items := make([]resource.Resource, 0, len(memoryIDs))
	for _, id := range memoryIDs {
		// A repeated id counts once, because one digest cannot push an item twice.
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

// Usage returns the usage records for these items, or all of them for no ids.
func (s *Store) Usage(ctx context.Context, memoryIDs []string) ([]state.MemoryUsage, error) {
	out, err := listByMemoryID(ctx, s, UsageKind, memoryIDs, toUsage, func(u state.MemoryUsage) string { return u.MemoryID })
	if err != nil {
		return nil, err
	}
	state.SortUsage(out)
	return out, nil
}

// listByMemoryID lists kind across scopes and keeps the records for memoryIDs, or
// all for none. Callers ask about a whole digest, and a per-item read would cost a
// lookup and a scoped list per item.
func listByMemoryID[T any](
	ctx context.Context,
	s *Store,
	kind string,
	memoryIDs []string,
	decode func(resource.Resource) (T, error),
	idOf func(T) string,
) ([]T, error) {
	rs, err := s.rs.ListAll(ctx, kind, nil)
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
	out := make([]T, 0, len(rs))
	for _, r := range rs {
		v, err := decode(r)
		if err != nil {
			return nil, err
		}
		if want != nil && !want[idOf(v)] {
			continue
		}
		out = append(out, v)
	}
	return out, nil
}

// mutateUsage applies mutate to this instance's record, creating it if needed. The
// read's sync version goes into the write, so a concurrent recorder loses the write
// and retries, and no increment is lost.
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
		// A store stamping another instance would file these counts under a name
		// nothing reads. Fail here, where the cause is one option.
		if out.OriginInstanceID != "" && out.OriginInstanceID != s.instanceID {
			return fmt.Errorf("memory: usage attributed to instance %q but this store records as %q: %w",
				out.OriginInstanceID, s.instanceID, state.ErrInvalid)
		}
		return nil
	}
	return lastErr
}

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

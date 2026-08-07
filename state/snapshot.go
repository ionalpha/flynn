package state

import (
	"encoding/json"
	"sort"
)

// Snapshot is a materialized projection of the state stream up to LastSeq: every
// session, its turns, every skill (live and tombstoned), every memory item, every
// per-instance usage row, and the derived skill-slug index, enough to restore the read
// model without folding the stream from the start. It is the state analogue of the resource snapshot, opaque to the spine
// and sealed by the same codec when one is configured. The slug index is carried so an
// in-memory restore is exact; a table-backed store rebuilds it from its own rows and
// leaves the field nil.
type Snapshot struct {
	LastSeq  int64             `json:"lastSeq"`
	Sessions []Session         `json:"sessions"`
	Turns    []Turn            `json:"turns"`
	Skills   []Skill           `json:"skills"`
	Items    []MemoryItem      `json:"items"`
	Usage    []MemoryUsage     `json:"usage,omitempty"`
	SlugToID map[string]string `json:"slugToID,omitempty"`
}

// MarshalSnapshot serializes a state projection into the payload a Replay or Rebuild
// restores from. The collections are sorted so the payload is deterministic for a given
// projection: sessions, skills, and items by id, turns by session then seq.
func MarshalSnapshot(s Snapshot) ([]byte, error) {
	sort.Slice(s.Sessions, func(i, j int) bool { return s.Sessions[i].ID < s.Sessions[j].ID })
	sort.Slice(s.Skills, func(i, j int) bool { return s.Skills[i].ID < s.Skills[j].ID })
	sort.Slice(s.Items, func(i, j int) bool { return s.Items[i].ID < s.Items[j].ID })
	SortUsage(s.Usage)
	sort.Slice(s.Turns, func(i, j int) bool {
		if s.Turns[i].SessionID != s.Turns[j].SessionID {
			return s.Turns[i].SessionID < s.Turns[j].SessionID
		}
		return s.Turns[i].Seq < s.Turns[j].Seq
	})
	return json.Marshal(s)
}

// UnmarshalSnapshot decodes a state snapshot payload, the inverse of
// MarshalSnapshot, so any backend restores from the one shared format.
func UnmarshalSnapshot(payload []byte) (Snapshot, error) {
	var s Snapshot
	err := json.Unmarshal(payload, &s)
	return s, err
}

// snapshotLocked captures the core's current projection as a Snapshot. The caller
// holds c.mu. The slug index is copied so an in-memory restore reproduces the exact
// read model, derived indices included, without replaying event types.
func (c *core) snapshotLocked() Snapshot {
	s := Snapshot{LastSeq: c.lastSeq, SlugToID: make(map[string]string, len(c.slugToID))}
	for _, ses := range c.sessions {
		s.Sessions = append(s.Sessions, ses)
	}
	for _, ts := range c.turns {
		s.Turns = append(s.Turns, ts...)
	}
	for _, sk := range c.skillsByID {
		s.Skills = append(s.Skills, sk)
	}
	s.Items = append(s.Items, c.memItems...)
	for _, u := range c.memUsage {
		s.Usage = append(s.Usage, u)
	}
	for k, v := range c.slugToID {
		s.SlugToID[k] = v
	}
	return s
}

// restoreLocked replaces the core's projection with a snapshot's, so a fold resumes from
// it and applies only the events after LastSeq. The caller holds c.mu. Per-session turn
// order is preserved because a sorted payload groups each session's turns in seq order.
func (c *core) restoreLocked(s Snapshot) {
	c.lastSeq = s.LastSeq
	c.sessions = make(map[string]Session, len(s.Sessions))
	c.turns = map[string][]Turn{}
	c.skillsByID = make(map[string]Skill, len(s.Skills))
	c.slugToID = make(map[string]string, len(s.SlugToID))
	c.memItems = nil
	c.memUsage = make(map[string]MemoryUsage, len(s.Usage))
	for _, u := range s.Usage {
		c.memUsage[usageKey(u.MemoryID, u.InstanceID)] = u
	}
	for _, ses := range s.Sessions {
		c.sessions[ses.ID] = ses
	}
	for _, t := range s.Turns {
		c.turns[t.SessionID] = append(c.turns[t.SessionID], t)
	}
	for _, sk := range s.Skills {
		c.skillsByID[sk.ID] = sk
	}
	for k, v := range s.SlugToID {
		c.slugToID[k] = v
	}
	c.memItems = append(c.memItems, s.Items...)
}

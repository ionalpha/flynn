package state

import (
	"encoding/json"
	"sort"

	"github.com/ionalpha/flynn/usage"
)

// Snapshot is the state projection up to LastSeq, enough to restore the read
// model without folding from the start. Skills include tombstoned ones. It is
// opaque to the spine and sealed by its codec when one is configured.
// SlugToID makes an in-memory restore exact; a table-backed store rebuilds
// the index from its rows and leaves it nil.
type Snapshot struct {
	LastSeq    int64             `json:"lastSeq"`
	Sessions   []Session         `json:"sessions"`
	Turns      []Turn            `json:"turns"`
	Skills     []Skill           `json:"skills"`
	Items      []MemoryItem      `json:"items"`
	Usage      []MemoryUsage     `json:"usage,omitempty"`
	Promotions []MemoryPromotion `json:"promotions,omitempty"`
	// ModelUsageRecords are the stored model usage [usage.Record]s, kept so a store
	// resuming from a snapshot does not lose the records before it.
	ModelUsageRecords []usage.Record    `json:"modelUsage,omitempty"`
	SlugToID          map[string]string `json:"slugToID,omitempty"`
}

// MarshalSnapshot sorts every collection before encoding, so a projection
// always yields the same bytes. Turns sort by session then seq.
func MarshalSnapshot(s Snapshot) ([]byte, error) {
	sort.Slice(s.Sessions, func(i, j int) bool { return s.Sessions[i].ID < s.Sessions[j].ID })
	sort.Slice(s.Skills, func(i, j int) bool { return s.Skills[i].ID < s.Skills[j].ID })
	sort.Slice(s.Items, func(i, j int) bool { return s.Items[i].ID < s.Items[j].ID })
	SortUsage(s.Usage)
	SortPromotions(s.Promotions)
	SortUsageRecords(s.ModelUsageRecords)
	sort.Slice(s.Turns, func(i, j int) bool {
		if s.Turns[i].SessionID != s.Turns[j].SessionID {
			return s.Turns[i].SessionID < s.Turns[j].SessionID
		}
		return s.Turns[i].Seq < s.Turns[j].Seq
	})
	return json.Marshal(s)
}

// UnmarshalSnapshot is the inverse of [MarshalSnapshot].
func UnmarshalSnapshot(payload []byte) (Snapshot, error) {
	var s Snapshot
	err := json.Unmarshal(payload, &s)
	return s, err
}

// snapshotLocked captures the projection; the caller holds c.mu.
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
	for _, pr := range c.memPromo {
		s.Promotions = append(s.Promotions, pr)
	}
	for _, r := range c.modelUsageRecs {
		s.ModelUsageRecords = append(s.ModelUsageRecords, r)
	}
	for k, v := range c.slugToID {
		s.SlugToID[k] = v
	}
	return s
}

// restoreLocked replaces the projection with s; the caller holds c.mu. Turn
// order holds because a marshalled payload is sorted by session then seq.
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
	c.memPromo = make(map[string]MemoryPromotion, len(s.Promotions))
	for _, pr := range s.Promotions {
		c.memPromo[pr.MemoryID] = pr
	}
	c.modelUsageRecs = make(map[modelUsageIdent]usage.Record, len(s.ModelUsageRecords))
	for _, r := range s.ModelUsageRecords {
		c.modelUsageRecs[modelUsageIdent{r.Run, r.Call}] = r
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

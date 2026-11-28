package state

import (
	"sort"
	"time"
)

// UsageOrigin records whether a session found a memory item itself or had it
// pushed in front of it. Pushed items look recalled every time, so decay on an
// undivided count would protect what the push already favours. Counting the
// two apart lets a selection policy see whether an item is used for itself.
type UsageOrigin string

const (
	// UsageOrganic is a use of an item the reader found: an explicit recall,
	// or a surfaced item acted on with no push behind it.
	UsageOrganic UsageOrigin = "organic"
	// UsagePrimed is a use of an item already pushed at the reader. It counts,
	// but says nothing about whether it would have been found unprompted.
	UsagePrimed UsageOrigin = "primed"
)

// Valid reports whether o is one of the two origins. There is no default: the
// caller knows which it is, and a guess would corrupt the measurement.
func (o UsageOrigin) Valid() bool { return o == UsageOrganic || o == UsagePrimed }

// MemoryUsage is how one instance has pushed and used one memory item. It is
// kept apart from the item, so MemoryItem stays append-only and its content
// hash never changes on a read.
//
// There is one row per (MemoryID, InstanceID), and an instance writes only its
// own. Replication is then conflict-free and summing rows is exact, where a
// shared last-writer-wins counter would drop increments; per-instance rows are
// also what [Monoculture] needs.
type MemoryUsage struct {
	// MemoryID is a plain id with no foreign key, because usage outlives a
	// tombstone and a curator still needs the deleted item's record.
	MemoryID   string
	InstanceID string
	// PushCount is how often this instance put the item in front of a reader
	// unasked. Pushed often and never used is what a selection policy demotes.
	PushCount int64
	// LastPushedAt is zero if never pushed.
	LastPushedAt time.Time
	// OrganicUses and PrimedUses split UseCount by [UsageOrigin].
	OrganicUses int64
	PrimedUses  int64
	// LastUsedAt is the last use of either origin, zero if never. It is separate
	// from LastPushedAt so a push never looks like a read.
	LastUsedAt time.Time
	Envelope
}

// UseCount is OrganicUses plus PrimedUses.
func (u MemoryUsage) UseCount() int64 { return u.OrganicUses + u.PrimedUses }

// Ignored reports whether the item was pushed and never used. Nothing in this
// package deletes an item for it.
func (u MemoryUsage) Ignored() bool { return u.PushCount > 0 && u.UseCount() == 0 }

// TotalUsage sums one item's per-instance rows: counters add, timestamps take
// the latest, and InstanceID is empty. It does not group by item, so the
// caller must pass one item's rows; mixed rows give a meaningless sum.
func TotalUsage(rows []MemoryUsage) MemoryUsage {
	var out MemoryUsage
	for _, r := range rows {
		out.MemoryID = r.MemoryID
		out.PushCount += r.PushCount
		out.OrganicUses += r.OrganicUses
		out.PrimedUses += r.PrimedUses
		if r.LastPushedAt.After(out.LastPushedAt) {
			out.LastPushedAt = r.LastPushedAt
		}
		if r.LastUsedAt.After(out.LastUsedAt) {
			out.LastUsedAt = r.LastUsedAt
		}
	}
	return out
}

// Monoculture is the mean pairwise Jaccard similarity of instances' pushed
// sets, in [0, 1]: 1 means every instance pushes the same set. A value rising
// toward 1 over a corpus that is not shrinking means the selection policy is
// homogenizing the fleet.
//
// Fewer than two pushing instances returns 0, since there is no overlap to
// measure. Agents sharing one instance count as one, so it under-reports
// there.
func Monoculture(rows []MemoryUsage) float64 {
	sets := map[string]map[string]bool{}
	for _, r := range rows {
		if r.PushCount == 0 || r.InstanceID == "" {
			continue
		}
		if sets[r.InstanceID] == nil {
			sets[r.InstanceID] = map[string]bool{}
		}
		sets[r.InstanceID][r.MemoryID] = true
	}
	if len(sets) < 2 {
		return 0
	}
	ids := make([]string, 0, len(sets))
	for id := range sets {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var sum float64
	var pairs int
	for i := range ids {
		for j := i + 1; j < len(ids); j++ {
			sum += jaccard(sets[ids[i]], sets[ids[j]])
			pairs++
		}
	}
	return sum / float64(pairs)
}

// jaccard is |a and b| over |a or b|; both sets are non-empty, so the union
// is never zero.
func jaccard(a, b map[string]bool) float64 {
	var inter int
	for k := range a {
		if b[k] {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	return float64(inter) / float64(union)
}

// SortUsage orders rows by item then instance, so every backend returns the
// same order.
func SortUsage(rows []MemoryUsage) {
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].MemoryID != rows[j].MemoryID {
			return rows[i].MemoryID < rows[j].MemoryID
		}
		return rows[i].InstanceID < rows[j].InstanceID
	})
}

package state

import (
	"sort"
	"time"
)

// UsageOrigin records why a memory item was used: whether the session went and
// found it, or whether it was already in front of the session because something
// had pushed it there.
//
// The split is the whole point of the field. A store that pushes memory at a
// session (a wake-time digest, a ride-along surfacing) destroys last-recalled-at
// as a signal on its own, because everything pushed is "recalled" every time.
// Decay driven by an undivided count would then protect exactly the items the
// push already favours and starve everything else, which is rich-get-richer with
// extra steps. Counting the two separately is what lets a selection policy ask
// the only question worth asking: is this item earning its place, or is it only
// here because we keep putting it here.
type UsageOrigin string

const (
	// UsageOrganic is a use of an item the reader went and found - an explicit
	// recall, or a surfaced item acted on with no push behind it.
	UsageOrganic UsageOrigin = "organic"
	// UsagePrimed is a use of an item that had already been pushed in front of
	// the reader. It is a real use and it counts, but it is not evidence the item
	// would have been found without the push.
	UsagePrimed UsageOrigin = "primed"
)

// Valid reports whether o is one of the two recognised origins. There is no
// default: a caller that records a use knows which of the two it is, and guessing
// on their behalf would quietly corrupt the one measurement this field exists for.
func (o UsageOrigin) Valid() bool { return o == UsageOrganic || o == UsagePrimed }

// MemoryUsage is how one instance has used one memory item: how often it has been
// pushed at a reader, how often it was actually used, and when each last happened.
// It is an observation about a record, never part of the record: MemoryItem stays
// append-only and its content hash never moves because something read it.
//
// One row per (MemoryID, InstanceID), and an instance only ever writes its own.
// That is what makes usage safe to replicate: two instances cannot conflict on a
// counter they do not share, so summing the rows is exact where a single shared
// counter merged last-writer-wins would silently drop increments. It is also what
// makes cross-instance overlap measurable at all (see Monoculture) - an aggregate
// counter would have already thrown that away.
type MemoryUsage struct {
	// MemoryID is the item this usage is about. It is a plain id, not a foreign
	// key: usage outlives a tombstone, so a curator reviewing what got pushed and
	// never used can still see the deleted item's record.
	MemoryID string
	// InstanceID is the instance whose reads this row counts.
	InstanceID string
	// PushCount is how many times this instance put the item in front of a reader
	// without being asked for it. Pushed often and used never is the signature of
	// noise, and it is the signal a selection policy demotes on.
	PushCount int64
	// LastPushedAt is when the last push happened, zero if never.
	LastPushedAt time.Time
	// OrganicUses and PrimedUses split UseCount by UsageOrigin. Organic uses are
	// the ones that say something about the item; primed uses mostly say something
	// about the push.
	OrganicUses int64
	PrimedUses  int64
	// LastUsedAt is when the item was last used, of either origin, zero if never.
	// It is deliberately a second timestamp rather than a reuse of LastPushedAt:
	// keeping them apart is what stops a push from looking like a read.
	LastUsedAt time.Time
	Envelope
}

// UseCount is the total number of uses of either origin.
func (u MemoryUsage) UseCount() int64 { return u.OrganicUses + u.PrimedUses }

// Ignored reports whether the item has been pushed and never used - the shape a
// selection policy demotes and a curator reviews. It is not a deletion signal:
// nothing in this package removes a memory item because it went unused.
func (u MemoryUsage) Ignored() bool { return u.PushCount > 0 && u.UseCount() == 0 }

// TotalUsage sums per-instance rows into the fleet-wide total for one item. The
// counters add because each instance counts only its own reads; the timestamps
// take the latest of any instance, and InstanceID is left empty because the sum
// belongs to no single instance.
//
// Rows for more than one item sum too, and the result is then meaningless, so the
// caller is expected to group first. TotalUsage does not group for them: an
// unasked-for grouping is how a helper ends up deciding a policy question.
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

// Monoculture is how alike the pushed sets of different instances are, as the mean
// pairwise Jaccard similarity over rows with a push on them: 0 when no two
// instances have pushed the same item, 1 when every instance pushes exactly the
// same set.
//
// It is the check on the selection policy rather than on the memory. A policy that
// homogenizes a fleet gives every instance the same handful of facts, and the
// fleet then converges on one way of seeing its work while looking, from inside
// each instance, like memory is doing its job. A number that climbs toward 1 over
// a corpus that is not itself shrinking is the policy's problem, not the corpus's.
//
// Fewer than two instances with pushes returns 0: there is no overlap to measure,
// and reporting a fabricated 1 for the single-instance case would read as maximum
// monoculture on exactly the setup where the metric means nothing.
//
// It measures instances, which is the identity this store records. A host that
// runs several agents inside one instance sees them here as one, and the metric
// under-reports for it.
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

// jaccard is the size of the intersection over the size of the union. Both sets
// are non-empty by construction here, so the union is never zero.
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

// SortUsage orders usage rows by item then instance, so every backend returns one
// deterministic order.
func SortUsage(rows []MemoryUsage) {
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].MemoryID != rows[j].MemoryID {
			return rows[i].MemoryID < rows[j].MemoryID
		}
		return rows[i].InstanceID < rows[j].InstanceID
	})
}

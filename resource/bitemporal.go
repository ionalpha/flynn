package resource

import "time"

// ValidAt reports whether the resource is true in the world at valid-time t.
// Valid-time (ValidFrom, ValidTo) is when a fact holds, set by the writer; event-time
// (UpdatedHLC and the log) is when it was recorded. Keeping them apart lets an agent
// record today a fact true since last week or from next week.
//
// The interval is half-open, [from, to), so adjacent intervals tile without
// overlap. A nil ValidFrom means the creation time, resolved here rather than stored
// so equal content recorded at different times still hashes equal; a nil ValidTo
// means no upper bound.
func (r Resource) ValidAt(t time.Time) bool {
	from := r.CreatedAt
	if r.ValidFrom != nil {
		from = *r.ValidFrom
	}
	if t.Before(from) {
		return false
	}
	if r.ValidTo != nil && !t.Before(*r.ValidTo) {
		return false
	}
	return true
}

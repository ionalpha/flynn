package resource

import "time"

// Bitemporal time: every resource carries two independent time axes.
//
//   - Event-time is when the system recorded a fact. It is the envelope's
//     UpdatedHLC and the ordering of the event log: replaying the log to a past
//     point reconstructs what we had recorded then.
//   - Valid-time is when a fact was or is true in the world, carried by the
//     ValidFrom and ValidTo envelope fields. It is set by the writer and is
//     independent of when we happened to record it.
//
// The two answer different questions. Event-time answers "what did we believe, and
// when did we learn otherwise" (the audit axis); valid-time answers "what was
// actually true as of some moment" (the world-state axis). An agent reasoning about
// a changing world needs both: it may record today a fact that became true last
// week (ValidFrom in the past), or that will hold from next week (ValidFrom in the
// future), without distorting the record of when it learned it.
//
// Valid-time is left unset (nil) for the common case, where a fact is taken to be
// valid from the moment it was recorded. Keeping the default nil rather than
// materialising it to the creation time preserves content-addressing: two records
// with identical content recorded at different times still hash equal (see Hash).
// ValidAt resolves the default at query time instead.

// ValidAt reports whether the resource is true in the world at valid-time t.
//
// The valid interval is half-open, [from, to): the instant a fact ceases to be
// true is excluded, so adjacent intervals tile without overlap. A nil ValidFrom
// defaults to the resource's creation time (valid-time tracks event-time when the
// writer did not say otherwise); a nil ValidTo means the fact is still valid, with
// no upper bound.
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

package instance

import (
	"time"

	"github.com/ionalpha/flynn/resource"
)

const (
	// ConditionReachable is the [resource.Condition] type [ReachableCondition]
	// reports: whether the process behind an Instance is still writing its record.
	ConditionReachable = "Reachable"

	// ReasonHeartbeatFresh is the reason on a Reachable condition whose heartbeat is
	// within the staleness window.
	ReasonHeartbeatFresh = "HeartbeatFresh"
	// ReasonHeartbeatStale is the reason on an unreachable condition: the heartbeat
	// is older than the window, so the process most likely stopped or lost its store.
	ReasonHeartbeatStale = "HeartbeatStale"
	// ReasonStopped is the reason on a Reachable=False condition for an instance
	// that recorded [StateDone]: it stopped deliberately, which is not a fault.
	ReasonStopped = "Stopped"
)

// Unreachable reports whether a control plane should mark the instance
// unreachable as of now: its heartbeat is older than staleAfter and it did not
// record a clean [StateDone] first. A cleanly stopped instance is not unreachable,
// it is finished, so a host badging runners does not flag a deliberate shutdown as
// a fault. It shares [IsStale]'s rules for the window, and is pure (time enters
// only through now).
func Unreachable(r resource.Resource, now time.Time, staleAfter time.Duration) bool {
	if st, err := DecodeStatus(r); err == nil && st.State == StateDone {
		return false
	}
	return IsStale(r, now, staleAfter)
}

// ReachableCondition derives the Reachable [resource.Condition] for an instance
// from its heartbeat age, so a host reads one standard shape instead of
// re-deriving the rule. Status is "True" while the heartbeat is fresh, and "False"
// with [ReasonHeartbeatStale] once [Unreachable] holds or [ReasonStopped] for a
// clean Done. LastTransitionTime is the heartbeat time (the envelope's last write),
// the latest moment the instance was known to be alive. The condition is computed
// on read and never stored, so it cannot itself go stale. It is pure.
func ReachableCondition(r resource.Resource, now time.Time, staleAfter time.Duration) resource.Condition {
	c := resource.Condition{Type: ConditionReachable, LastTransitionTime: r.UpdatedAt}
	switch {
	case Unreachable(r, now, staleAfter):
		c.Status, c.Reason = "False", ReasonHeartbeatStale
	case IsStale(r, now, staleAfter):
		c.Status, c.Reason = "False", ReasonStopped
	default:
		c.Status, c.Reason = "True", ReasonHeartbeatFresh
	}
	return c
}

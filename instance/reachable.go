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
)

// Unreachable reports whether a control plane should mark the instance
// unreachable as of now: [IsStale] holds for its heartbeat. A recorded
// [StateDone] does not exempt it. Done is written by the instance itself, so
// exempting it would let a forged Done hide a runner that is still being driven;
// a host that wants to tell a finished runner from a lost one reads the state as
// well. A non-positive staleAfter uses [DefaultStaleAfter]. It is pure (time
// enters only through now).
func Unreachable(r resource.Resource, now time.Time, staleAfter time.Duration) bool {
	return IsStale(r, now, staleAfter)
}

// ReachableCondition derives the Reachable [resource.Condition] for an instance
// from its heartbeat age, so a host reads one standard shape instead of
// re-deriving the rule. Status is "True" with [ReasonHeartbeatFresh] while the
// heartbeat is fresh and "False" with [ReasonHeartbeatStale] once [Unreachable]
// holds. LastTransitionTime is the heartbeat time (the envelope's last write), the
// latest moment the instance was known to be alive. The condition is computed on
// read and never stored, so it cannot itself go stale. It is pure.
func ReachableCondition(r resource.Resource, now time.Time, staleAfter time.Duration) resource.Condition {
	c := resource.Condition{Type: ConditionReachable, LastTransitionTime: r.UpdatedAt}
	if Unreachable(r, now, staleAfter) {
		c.Status, c.Reason = "False", ReasonHeartbeatStale
	} else {
		c.Status, c.Reason = "True", ReasonHeartbeatFresh
	}
	return c
}

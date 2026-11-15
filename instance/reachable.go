package instance

import (
	"time"

	"github.com/ionalpha/flynn/resource"
)

const (
	// ConditionReachable is the [resource.Condition] type [ReachableCondition]
	// reports.
	ConditionReachable = "Reachable"

	// ReasonHeartbeatFresh is the reason while the heartbeat is fresh.
	ReasonHeartbeatFresh = "HeartbeatFresh"
	// ReasonHeartbeatStale means the process most likely stopped or lost its
	// store.
	ReasonHeartbeatStale = "HeartbeatStale"
)

// Unreachable reports whether a control plane should mark the instance
// unreachable: [IsStale] holds. A recorded [StateDone] does not exempt it,
// because the instance writes Done itself and a forged one could hide a runner
// still being driven. A host that tells finished from lost reads the state too.
func Unreachable(r resource.Resource, now time.Time, staleAfter time.Duration) bool {
	return IsStale(r, now, staleAfter)
}

// ReachableCondition is the Reachable [resource.Condition]: "False" once
// [Unreachable] holds, else "True". LastTransitionTime is the heartbeat, the
// last moment the instance was known alive. It is computed on read and never
// stored, so it cannot go stale.
func ReachableCondition(r resource.Resource, now time.Time, staleAfter time.Duration) resource.Condition {
	c := resource.Condition{Type: ConditionReachable, LastTransitionTime: r.UpdatedAt}
	if Unreachable(r, now, staleAfter) {
		c.Status, c.Reason = "False", ReasonHeartbeatStale
	} else {
		c.Status, c.Reason = "True", ReasonHeartbeatFresh
	}
	return c
}

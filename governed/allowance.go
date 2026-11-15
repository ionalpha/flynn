package governed

import (
	"github.com/ionalpha/flynn/allowance"
	"github.com/ionalpha/flynn/mission"
)

// outsidePolicy marks nothing by default. The waist sees an action's name and not its
// arguments, and one name covers both listing and deleting, so only the operator can say
// which actions reach outside.
func outsidePolicy(actions []string) allowance.Actions { return allowance.NewActions(actions...) }

// outsideGate returns nil when nothing is marked, so [Posture.Allowance] does not claim
// a gate that marks nothing.
func outsideGate(actions []string) allowance.Policy {
	policy := outsidePolicy(actions)
	if len(policy) == 0 {
		return nil
	}
	return policy
}

// allowanceOptions installs the gate only when an action is marked.
func allowanceOptions(outside []string) []mission.Option {
	policy := outsidePolicy(outside)
	if len(policy) == 0 {
		return nil
	}
	return []mission.Option{mission.WithAllowance(policy)}
}

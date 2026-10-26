package governed

import (
	"github.com/ionalpha/flynn/allowance"
	"github.com/ionalpha/flynn/mission"
)

// outsidePolicy turns a list of action names into the gate's policy: the actions whose
// effects leave the workspace and cannot be taken back. Blank entries are dropped and
// duplicates collapse.
//
// An empty result is the default and marks nothing. Which actions reach outside the
// workspace is the operator's to say rather than the binary's to guess: the waist
// governs an action's identity and never its arguments, and one action name covers both
// a command that lists a directory and a command that deletes something that was not
// backed up. A default list would therefore be a list of guesses, and the two ways of
// being wrong are not symmetrical: marking too much stops runs that were fine, marking
// too little is a gate that reads as present and is not.
func outsidePolicy(actions []string) allowance.Actions { return allowance.NewActions(actions...) }

// outsideGate returns the policy as the driver spec's port, or nil when nothing is
// marked, so a run with no marked action carries no gate at all rather than one that is
// installed and marks nothing. Both admit everything; only one claims a gate is there,
// and [Posture.Allowance] reports the claim.
func outsideGate(actions []string) allowance.Policy {
	policy := outsidePolicy(actions)
	if len(policy) == 0 {
		return nil
	}
	return policy
}

// allowanceOptions returns the mission options that install the gate, or none when no
// action is marked. The gate refuses a marked action the goal does not declare, and the
// reconciler turns that refusal into the ask it hands the run's author.
func allowanceOptions(outside []string) []mission.Option {
	policy := outsidePolicy(outside)
	if len(policy) == 0 {
		return nil
	}
	return []mission.Option{mission.WithAllowance(policy)}
}

package main

import (
	"sort"
	"strings"

	"github.com/ionalpha/flynn/allowance"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/mission"
)

// outsidePolicy turns the operator's list of action names into the gate's policy: the
// actions whose effects leave the workspace and cannot be taken back. Blank entries are
// dropped and duplicates collapse.
//
// An empty result is the default and marks nothing, so a run behaves exactly as it did
// before. Which actions reach outside the workspace is the operator's to say rather than
// the binary's to guess: the waist governs an action's identity and never its arguments,
// and one action name covers both a command that lists a directory and a command that
// deletes something that was not backed up. A default list would therefore be a list of
// guesses, and the two ways of being wrong are not symmetrical - marking too much stops
// runs that were fine, marking too little is a gate that reads as present and is not.
func outsidePolicy(actions []string) allowance.Actions { return allowance.NewActions(actions...) }

// outsideGate returns the policy as the driver spec's port, or nil when the operator marked
// nothing, so a run with no marked action carries no gate at all rather than one that is
// installed and marks nothing. The distinction matters to a reader of the assembly more
// than to the run: both admit everything, and only one of them claims a gate is there.
func outsideGate(actions []string) allowance.Policy {
	policy := outsidePolicy(actions)
	if len(policy) == 0 {
		return nil
	}
	return policy
}

// allowanceOptions returns the mission options that install the gate, or none when the
// operator marked no action. The gate refuses a marked action the goal does not declare,
// and the reconciler turns that refusal into the ask it hands the run's author.
func (s gateSetup) allowanceOptions() []mission.Option {
	policy := outsidePolicy(s.outside)
	if len(policy) == 0 {
		return nil
	}
	return []mission.Option{mission.WithAllowance(policy)}
}

// declaredAllowances turns the operator's list of allowed action names into the standing
// authorizations carried on the goal spec. Blank entries are dropped and duplicates
// collapse, so declaring one action twice is one authorization.
//
// Every declaration made this way is action-level: it authorizes the action against
// whatever it is attempted on. A declaration narrowed to one target is expressible on the
// spec and is not offered here, because nothing on the CLI's own path binds a target for
// the gate to match it against, and a flag whose narrower form silently authorizes nothing
// would be worse than not having the form.
func declaredAllowances(actions []string) []goal.Allowance {
	seen := map[string]bool{}
	names := make([]string, 0, len(actions))
	for _, a := range actions {
		if name := strings.TrimSpace(a); name != "" && !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	sort.Strings(names)
	out := make([]goal.Allowance, 0, len(names))
	for _, name := range names {
		out = append(out, goal.Allowance{Action: name})
	}
	return out
}
